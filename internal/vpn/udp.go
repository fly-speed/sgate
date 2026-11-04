package vpn

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/pion/dtls/v3"
	"github.com/quic-go/quic-go"
)

const udpProtocol = "sgate-vpn-udp-v1"
const udpReliableControlProtocol = "sgate-vpn-udp-control-v1"
const udpHeaderSize = 8
const udpFrameMax = 1200
const udpReceiveBuffer = 1024 * 1024
const (
	udpControl byte = 1 + iota
	udpPacket
	udpKeepalive
	udpClose
	udpControlFIN
	udpControlACK
)

func udpSecurity(key []byte) []dtls.Option {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(udpProtocol))
	psk := m.Sum(nil)
	return []dtls.Option{
		dtls.WithPSK(func(hint []byte) ([]byte, error) {
			if !hmac.Equal(hint, []byte(udpProtocol)) {
				return nil, errors.New("invalid UDP VPN identity")
			}
			return append([]byte(nil), psk...), nil
		}),
		dtls.WithPSKIdentityHint([]byte(udpProtocol)),
		dtls.WithCipherSuites(dtls.TLS_PSK_WITH_AES_128_GCM_SHA256),
		dtls.WithExtendedMasterSecret(dtls.RequireExtendedMasterSecret),
		dtls.WithSupportedProtocols(udpReliableControlProtocol, udpProtocol),
		dtls.WithReplayProtectionWindow(4096),
		dtls.WithMTU(udpFrameMax),
		// Retry handshake flights with DTLS backoff within the 8-second
		// establishment deadline. Application records are sent separately.
		dtls.WithFlightInterval(time.Second),
	}
}

// UDP business records never carry acknowledgements or request retransmits.
// Epochs, per-direction nonces, encryption and replay checks belong to DTLS.
func encodeUDPFrame(kind byte, p []byte) ([]byte, error) {
	if !validUDPFrame(kind, len(p)) {
		return nil, errors.New("invalid UDP VPN frame")
	}
	b := make([]byte, udpHeaderSize+len(p))
	copy(b, []byte{'S', 'G', 'U', 1})
	b[4] = kind
	binary.BigEndian.PutUint16(b[6:8], uint16(len(p)))
	copy(b[8:], p)
	return b, nil
}
func decodeUDPFrame(b []byte) (byte, []byte, error) {
	if len(b) < udpHeaderSize || string(b[:4]) != "SGU\x01" || b[5] != 0 || int(binary.BigEndian.Uint16(b[6:8])) != len(b)-udpHeaderSize || !validUDPFrame(b[4], len(b)-udpHeaderSize) {
		return 0, nil, errors.New("invalid UDP VPN frame")
	}
	return b[4], b[8:], nil
}

func validUDPFrame(kind byte, length int) bool {
	switch kind {
	case udpControl, udpPacket:
		return length <= udpFrameMax-udpHeaderSize
	case udpKeepalive, udpClose, udpControlFIN:
		return length == 0
	case udpControlACK:
		return length == 4
	default:
		return false
	}
}

type udpConnection struct {
	raw            net.Conn
	ctx            context.Context
	cancel         context.CancelFunc
	reader, writer net.Conn
	control        *udpControlChannel
	reliable       *udpReliableControl
	packets        chan []byte
	stats          *Stats
	writeMu        sync.Mutex
	writeDeadline  time.Time
	once           sync.Once
}

func newUDPConnection(raw *dtls.Conn, stats *Stats) *udpConnection {
	ctx, cancel := context.WithCancel(context.Background())
	r, w := net.Pipe()
	c := &udpConnection{raw: raw, ctx: ctx, cancel: cancel, reader: r, writer: w, packets: make(chan []byte, queueSize), stats: stats}
	c.control = &udpControlChannel{c: c}
	if state, ok := raw.ConnectionState(); ok && state.NegotiatedProtocol == udpReliableControlProtocol {
		c.reliable = &udpReliableControl{incoming: make(chan []byte, 2), acks: make(chan uint32, 4)}
		go c.deliverUDPControl()
	}
	go c.readFrames()
	go c.keepAlive()
	return c
}
func (c *udpConnection) Context() context.Context { return c.ctx }
func (c *udpConnection) RemoteAddr() net.Addr     { return c.raw.RemoteAddr() }
func (c *udpConnection) supportsPackets() bool    { return true }
func (c *udpConnection) OpenStreamSync(ctx context.Context) (controlChannel, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.control, nil
}
func (c *udpConnection) AcceptStream(ctx context.Context) (controlChannel, error) {
	return c.OpenStreamSync(ctx)
}
func (c *udpConnection) finish() {
	c.once.Do(func() { c.cancel(); c.reader.Close(); c.writer.Close(); c.raw.Close() })
}
func (c *udpConnection) write(kind byte, p []byte) error {
	b, err := encodeUDPFrame(kind, p)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.ctx.Err() != nil {
		return io.EOF
	}
	deadline := time.Now().Add(2 * time.Second)
	if !c.writeDeadline.IsZero() && c.writeDeadline.Before(deadline) {
		deadline = c.writeDeadline
	}
	if err = c.raw.SetWriteDeadline(deadline); err != nil {
		return err
	}
	n, err := c.raw.Write(b) // Exactly one application write. Never replay IP data.
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	if err == nil && kind == udpPacket {
		c.stats.udpTX.Add(1)
	}
	return err
}
func (c *udpConnection) SendDatagram(p []byte) error { return c.write(udpPacket, p) }
func (c *udpConnection) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	select {
	case p := <-c.packets:
		return p, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.ctx.Done():
		return nil, io.EOF
	}
}
func (c *udpConnection) CloseWithError(quic.ApplicationErrorCode, string) error {
	if c.ctx.Err() == nil {
		c.write(udpClose, nil)
	}
	c.finish()
	return nil
}
func (c *udpConnection) readFrames() {
	defer c.finish()
	b := make([]byte, 65535)
	for {
		c.raw.SetReadDeadline(time.Now().Add(60 * time.Second))
		n, err := c.raw.Read(b)
		if err != nil {
			return
		}
		kind, p, err := decodeUDPFrame(b[:n])
		if err != nil {
			c.stats.drops.Add(1)
			continue
		}
		switch kind {
		case udpControl:
			if c.reliable != nil {
				c.receiveUDPControl(p)
				continue
			}
			if _, err = c.writer.Write(p); err != nil {
				return
			}
		case udpControlACK:
			if c.reliable != nil {
				select {
				case c.reliable.acks <- binary.BigEndian.Uint32(p):
				default:
				}
			}
		case udpControlFIN:
			if c.reliable == nil {
				c.writer.Close()
			} else {
				// Deliver any acknowledged control data before EOF.
				select {
				case c.reliable.incoming <- nil:
				default:
				}
			}
		case udpPacket:
			c.stats.udpRX.Add(1)
			select {
			case c.packets <- append([]byte(nil), p...):
			default:
				c.stats.drops.Add(1)
				c.stats.udpQueueDrops.Add(1)
			}
		case udpKeepalive: // One-way NAT keepalive, no ACK or replay.
		case udpClose:
			return
		}
	}
}
func (c *udpConnection) keepAlive() {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-t.C:
			if c.write(udpKeepalive, nil) != nil {
				c.finish()
				return
			}
		}
	}
}

type udpControlChannel struct{ c *udpConnection }

func (c *udpControlChannel) Read(p []byte) (int, error) { return c.c.reader.Read(p) }
func (c *udpControlChannel) Write(p []byte) (int, error) {
	if c.c.reliable != nil {
		return c.c.writeUDPControl(p)
	}
	if err := c.c.write(udpControl, p); err != nil {
		return 0, err
	}
	return len(p), nil
}
func (c *udpControlChannel) Close() error { return c.c.write(udpControlFIN, nil) }
func (c *udpControlChannel) SetDeadline(t time.Time) error {
	c.c.writeMu.Lock()
	c.c.writeDeadline = t
	c.c.writeMu.Unlock()
	return c.c.reader.SetReadDeadline(t)
}
func (c *udpControlChannel) SetReadDeadline(t time.Time) error { return c.c.reader.SetReadDeadline(t) }
func (c *udpControlChannel) CancelRead(quic.StreamErrorCode)   { c.c.reader.Close() }

func dialUDP(ctx context.Context, c Config) (connection, error) {
	address, err := net.ResolveUDPAddr("udp", c.Address)
	if err != nil {
		return nil, err
	}
	socket, err := net.ListenUDP("udp", nil)
	if err != nil {
		return nil, err
	}
	// A lower OS cap must not prevent VPN startup.
	_ = socket.SetReadBuffer(udpReceiveBuffer)
	security := udpSecurity(c.Key)
	options := make([]dtls.ClientOption, len(security))
	for i, option := range security {
		options[i] = option
	}
	raw, err := dtls.ClientWithOptions(socket, address, options...)
	if err != nil {
		socket.Close()
		return nil, err
	}
	authCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if err = raw.HandshakeContext(authCtx); err != nil {
		raw.Close()
		return nil, err
	}
	return newUDPConnection(raw, c.Stats), nil
}
