package vpn

import (
	"bytes"
	"context"
	"crypto/hmac"
	"errors"
	"io"
	"net/netip"
	"strings"
	"time"

	"github.com/quic-go/quic-go"
)

const probeDatagramPrefix = "sgate-vpn-probe-v1:"

type ProbeResult struct {
	OK                 bool   `json:"ok"`
	Stage              string `json:"stage"`
	Detail             string `json:"detail"`
	Error              string `json:"error,omitempty"`
	Target             string `json:"target,omitempty"`
	TestID             string `json:"test_id,omitempty"`
	ElapsedMS          int64  `json:"elapsed_ms"`
	AddressInUse       bool   `json:"address_in_use,omitempty"`
	ClientLimitReached bool   `json:"client_limit_reached,omitempty"`
}

// Probe authenticates a separate connection and echoes an encrypted datagram.
// It never opens a device, configures networking, reserves an IP or invokes Connected.
func Probe(parent context.Context, config Config) (result ProbeResult) {
	started := time.Now()
	result.Stage = "config"
	defer func() { result.ElapsedMS = time.Since(started).Milliseconds() }()
	c, err := config.normalized(false)
	if err != nil {
		result.Error = err.Error()
		return
	}
	result.Target = c.Address
	result.TestID, err = randomNonce()
	if err != nil {
		result.Error = "cannot generate VPN test nonce"
		return
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	serverAdvertisedProbe := false
	fail := func(err error) ProbeResult {
		result.Error = err.Error()
		if result.Stage == "auth" && result.Detail == "" {
			var remote *quic.ApplicationError
			switch {
			case errors.As(err, &remote) && remote.ErrorCode == authKeyMismatchCode:
				result.Detail = "服务端确认 VPN 共享密钥不匹配；请核对两端生效的 vpn-key-file，留空时核对 key-file 及命令行覆盖。"
			case errors.As(err, &remote) && remote.ErrorCode == authInvalidHelloCode:
				result.Detail = "服务端无法解析 VPN 认证请求；请核对两端程序版本。"
			case !serverAdvertisedProbe:
				result.Detail = "服务端未声明新版测试能力；请核对实际监听 VPN 端口的服务端版本，并检查两端生效的共享密钥。"
			default:
				result.Detail = "VPN 认证期间连接断开；请查看服务端对应时刻的认证拒绝日志。"
			}
		}
		return result
	}
	result.Stage = "quic"
	conn, err := quic.DialAddr(ctx, c.Address, c.TLS, quicConfig())
	if err != nil {
		return fail(err)
	}
	defer conn.CloseWithError(0, "VPN test completed")
	if !conn.ConnectionState().SupportsDatagrams {
		return fail(errors.New("server did not negotiate QUIC DATAGRAM"))
	}
	result.Stage = "auth"
	control, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return fail(err)
	}
	deadline, _ := ctx.Deadline()
	control.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { conn.CloseWithError(0, "VPN test cancelled") })
	defer stop()
	if _, err = control.Write([]byte{1}); err != nil {
		return fail(err)
	}
	var challenge string
	if err = readJSON(control, &challenge); err != nil {
		return fail(err)
	}
	serverAdvertisedProbe = strings.HasPrefix(challenge, probeChallengePrefix)
	h := hello{Probe: true, ID: result.TestID, IP: c.ClientIP.String(), Network: c.Network.String(), MTU: c.MTU}
	h.Proof = proof(c.Key, challenge, h)
	if err = writeJSON(control, h); err != nil {
		return fail(err)
	}
	var reply welcome
	if err = readJSON(control, &reply); err != nil {
		return fail(err)
	}
	if reply.Error != "" {
		result.Detail = "服务端已通过密钥校验，但拒绝了 VPN 参数；请按错误中的期望值修正。"
		return fail(errors.New(reply.Error))
	}
	if !reply.Probe || !hmac.Equal([]byte(reply.Proof), []byte(welcomeProof(c.Key, challenge, h, reply))) {
		result.Detail = "服务端测试身份校验失败；请核对共享密钥及测试协议版本。"
		return fail(errors.New("server did not authenticate a non-disruptive VPN test; update server or check shared key"))
	}
	if reply.IP != h.IP || reply.Network != h.Network || reply.MTU != h.MTU || reply.Gateway != c.Network.Addr().Next().String() {
		return fail(errors.New("invalid VPN test assignment"))
	}
	result.Stage = "datagram"
	packet := []byte(probeDatagramPrefix + result.TestID)
	// DATAGRAM is intentionally unreliable. Retry the tiny echo within the total
	// deadline so a single lost packet does not turn into a ten-second timeout.
	for {
		if err = conn.SendDatagram(packet); err != nil {
			return fail(err)
		}
		receiveCtx, receiveCancel := context.WithTimeout(ctx, time.Second)
		echo, e := conn.ReceiveDatagram(receiveCtx)
		receiveCancel()
		if e == nil && bytes.Equal(echo, packet) {
			break
		}
		if ctx.Err() != nil {
			return fail(ctx.Err())
		}
		if e != nil && !errors.Is(e, context.DeadlineExceeded) {
			return fail(e)
		}
	}
	if _, err = control.Write([]byte{0}); err != nil {
		return fail(err)
	}
	result.OK = true
	result.AddressInUse = reply.AddressInUse
	result.ClientLimitReached = reply.ClientLimitReached
	result.Detail = "QUIC、证书、共享密钥认证及 DATAGRAM 回显通过；未测试 TUN、NAT 或互联网转发。"
	return result
}
func (s *Server) serveProbe(ctx context.Context, conn *quic.Conn, control *quic.Stream, challenge string, h hello, ip netip.Addr) {
	s.mu.Lock()
	occupied := s.peers[ip] != nil
	full := len(s.peers) >= s.cfg.MaxClients
	s.mu.Unlock()
	reply := welcome{Probe: true, IP: ip.String(), Network: s.cfg.Network.String(), Gateway: s.cfg.Network.Addr().Next().String(), MTU: s.cfg.MTU, AddressInUse: occupied, ClientLimitReached: full}
	reply.Proof = welcomeProof(s.cfg.Key, challenge, h, reply)
	if writeJSON(control, reply) != nil {
		return
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); var ack [1]byte; _, _ = io.ReadFull(control, ack[:]); cancel() }()
	defer func() { control.CancelRead(0); <-done }()
	for {
		packet, err := conn.ReceiveDatagram(ctx)
		if err != nil {
			return
		}
		if !bytes.Equal(packet, []byte(probeDatagramPrefix+h.ID)) {
			return
		}
		if conn.SendDatagram(packet) != nil {
			return
		}
	}
}
