// Package vpn carries IPv4 packets over authenticated QUIC datagrams.
package vpn

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/logging"
)

const Protocol = "sgate-vpn-v1"
const fragmentPayload = 1000
const headerSize = 14
const queueSize = 64

type Device interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
	Close() error
}
type Config struct {
	Address            string
	TLS                *tls.Config
	Key                []byte
	Network            netip.Prefix
	ClientIP           netip.Addr
	MTU                int
	MaxClients         int
	AllowPrivate       bool
	Noise              NoiseConfig
	QueueMaxAge        time.Duration
	RetryMin, RetryMax time.Duration
	// Called after authentication, once per client process. Cleanup runs on exit.
	Connected func() (func(), error)
	Stats     *Stats
	Logf      func(string, ...any)
}

func (c Config) normalized(server bool) (Config, error) {
	_, port, err := net.SplitHostPort(c.Address)
	if err != nil {
		return c, errors.New("VPN address must be host:port")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 || (!server && n == 0) {
		return c, errors.New("invalid VPN port")
	}
	if len(c.Key) != 32 {
		return c, errors.New("VPN requires a 32-byte shared key")
	}
	if c.TLS == nil {
		return c, errors.New("VPN requires TLS configuration")
	}
	if !c.Network.IsValid() {
		c.Network = netip.MustParsePrefix("10.77.0.0/24")
	}
	if !c.Network.Addr().Is4() || c.Network.Bits() < 16 || c.Network.Bits() > 30 || c.Network != c.Network.Masked() {
		return c, errors.New("VPN network must be a canonical IPv4 /16../30 prefix")
	}
	if c.MTU == 0 {
		c.MTU = 1280
	}
	if c.MTU < 576 || c.MTU > 1500 {
		return c, errors.New("VPN MTU must be 576..1500")
	}
	if c.MaxClients == 0 {
		c.MaxClients = 64
	}
	if c.MaxClients < 1 || c.MaxClients > 254 {
		return c, errors.New("VPN max-clients must be 1..254")
	}
	if !server && (!c.ClientIP.IsValid() || !validClientIP(c.Network, c.ClientIP)) {
		return c, errors.New("client-ip must be a host in vpn-network, excluding network, server and broadcast addresses")
	}
	if c.RetryMin == 0 {
		c.RetryMin = time.Second
	}
	if c.RetryMax == 0 {
		c.RetryMax = 10 * time.Second
	}
	if c.RetryMin <= 0 || c.RetryMax < c.RetryMin {
		return c, errors.New("invalid VPN retry interval")
	}
	if c.QueueMaxAge < 0 || c.QueueMaxAge > time.Minute {
		return c, errors.New("VPN queue maximum age must be 0..1m; 0 disables age expiry")
	}
	if c.Noise.Rate < 0 || c.Noise.Rate > maxNoiseRate || c.Noise.IdleAfter < 0 {
		return c, errors.New("invalid VPN noise rate (0..10M bytes/s) or idle threshold")
	}
	if c.Noise.IdleAfter == 0 {
		c.Noise.IdleAfter = time.Second
	}
	if c.Stats == nil {
		c.Stats = &Stats{}
	}
	c.TLS = c.TLS.Clone()
	c.TLS.MinVersion = tls.VersionTLS13
	c.TLS.NextProtos = []string{Protocol}
	if c.TLS.InsecureSkipVerify {
		return c, errors.New("VPN certificate verification cannot be disabled")
	}
	return c, nil
}
func validClientIP(network netip.Prefix, ip netip.Addr) bool {
	if !ip.Is4() || !network.Contains(ip) || ip == network.Addr() || ip == network.Addr().Next() {
		return false
	}
	b := ip.As4()
	base := network.Addr().As4()
	n := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	v := uint32(base[0])<<24 | uint32(base[1])<<16 | uint32(base[2])<<8 | uint32(base[3])
	return n != v+uint32(1<<(32-network.Bits()))-1
}
func quicConfig(stats ...*Stats) *quic.Config {
	config := &quic.Config{EnableDatagrams: true, InitialPacketSize: 1200, HandshakeIdleTimeout: 10 * time.Second, MaxIdleTimeout: 60 * time.Second, KeepAlivePeriod: 10 * time.Second, MaxIncomingStreams: 1, MaxIncomingUniStreams: -1, InitialStreamReceiveWindow: 8192, MaxStreamReceiveWindow: 8192, InitialConnectionReceiveWindow: 8192, MaxConnectionReceiveWindow: 8192}
	if len(stats) > 0 && stats[0] != nil {
		s := stats[0]
		config.Tracer = func(ctx context.Context, _ logging.Perspective, _ quic.ConnectionID) *logging.ConnectionTracer {
			flow, _ := ctx.Value(flowContextKey{}).(*datagramFlow)
			return &logging.ConnectionTracer{
				SentShortHeaderPacket: func(_ *logging.ShortHeader, _ logging.ByteCount, _ logging.ECN, _ *logging.AckFrame, frames []logging.Frame) {
					if flow != nil {
						for _, frame := range frames {
							if _, ok := frame.(*logging.DatagramFrame); ok {
								flow.sent.Add(1)
								flow.signal()
							}
						}
					}
				},
				UpdatedMetrics: func(rtt *logging.RTTStats, cwnd logging.ByteCount, inflight logging.ByteCount, _ int) {
					if flow != nil {
						flow.cwnd.Store(int64(cwnd))
						flow.inflight.Store(int64(inflight))
					}
					s.cwnd.Store(int64(cwnd))
					s.inflight.Store(int64(inflight))
					s.rtt.Store(int64(rtt.SmoothedRTT()))
				},
				LostPacket: func(logging.EncryptionLevel, logging.PacketNumber, logging.PacketLossReason) { s.lost.Add(1) },
				UpdatedPTOCount: func(n uint32) {
					if n > 0 {
						s.pto.Add(1)
					}
				},
			}
		}
	}
	return config
}

type Stats struct {
	mssClamped, fragmentExpired, fragmentEvicted              atomic.Uint64
	cwnd, inflight                                            atomic.Int64
	queuePeak, queueWaitMax, queueExpired, queueOverflow      atomic.Uint64
	tunWriteMax, admissionBlocked, sendWaitMax, admissionWait atomic.Uint64
	rtt                                                       atomic.Int64
	lost, pto                                                 atomic.Uint64
	tx, rx, drops, auth, connections                          atomic.Uint64
	active                                                    atomic.Int64
	noiseTX, noiseRX, noiseSkipped                            atomic.Uint64
	noiseUnsupported                                          atomic.Int64
}
type Snapshot struct {
	MSSClamped      uint64 `json:"tcp_mss_clamped_packets"`
	FragmentExpired uint64 `json:"fragment_reassembly_expired_packets"`
	FragmentEvicted uint64 `json:"fragment_reassembly_evicted_packets"`

	CongestionWindow int64   `json:"last_congestion_window_bytes"`
	Inflight         int64   `json:"last_bytes_in_flight"`
	TunWriteMaxMS    float64 `json:"tun_delivery_max_wait_ms"`
	AdmissionBlocked uint64  `json:"send_admission_blocked_events"`

	QueuePeak       uint64  `json:"send_queue_peak_packets"`
	QueueWaitMaxMS  float64 `json:"send_queue_max_wait_ms"`
	QueueExpired    uint64  `json:"send_queue_expired_packets"`
	QueueOverflow   uint64  `json:"send_queue_overflow_packets"`
	SendWaitMaxMS   float64 `json:"send_datagram_max_wait_ms"`
	AdmissionWaitMS float64 `json:"send_admission_wait_ms"`

	NoiseTX          uint64  `json:"noise_tx_bytes"`
	NoiseRX          uint64  `json:"noise_rx_bytes"`
	NoiseSkipped     uint64  `json:"noise_skipped_ticks"`
	NoiseUnsupported int64   `json:"noise_unsupported_connections"`
	RTT              float64 `json:"last_smoothed_rtt_ms"`
	Lost             uint64  `json:"quic_lost_packets"`
	PTO              uint64  `json:"quic_pto_events"`
	TX               uint64  `json:"tx_ip_bytes"`
	RX               uint64  `json:"rx_ip_bytes"`
	Drops            uint64  `json:"dropped_packets"`
	AuthRejected     uint64  `json:"auth_rejected"`
	Connections      uint64  `json:"connections_total"`
	Active           int64   `json:"active_connections"`
}

func (s *Stats) Snapshot() Snapshot {
	return Snapshot{MSSClamped: s.mssClamped.Load(), FragmentExpired: s.fragmentExpired.Load(), FragmentEvicted: s.fragmentEvicted.Load(), CongestionWindow: s.cwnd.Load(), Inflight: s.inflight.Load(), TunWriteMaxMS: float64(s.tunWriteMax.Load()) / 1e6, AdmissionBlocked: s.admissionBlocked.Load(), QueuePeak: s.queuePeak.Load(), QueueWaitMaxMS: float64(s.queueWaitMax.Load()) / 1e6, QueueExpired: s.queueExpired.Load(), QueueOverflow: s.queueOverflow.Load(), SendWaitMaxMS: float64(s.sendWaitMax.Load()) / 1e6, AdmissionWaitMS: float64(s.admissionWait.Load()) / 1e6, NoiseTX: s.noiseTX.Load(), NoiseRX: s.noiseRX.Load(), NoiseSkipped: s.noiseSkipped.Load(), NoiseUnsupported: s.noiseUnsupported.Load(), RTT: float64(s.rtt.Load()) / 1e6, Lost: s.lost.Load(), PTO: s.pto.Load(), TX: s.tx.Load(), RX: s.rx.Load(), Drops: s.drops.Load(), AuthRejected: s.auth.Load(), Connections: s.connections.Load(), Active: s.active.Load()}
}

// ValidateConfig permits command entrypoints to reject unsafe input before TUN setup.
func ValidateConfig(c Config, server bool) error { _, err := c.normalized(server); return err }
