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
const udpHeaderSize = 8
const udpFrameMax = 1200
const (
	udpControl byte = 1 + iota
	udpPacket
	udpKeepalive
	udpClose
	udpControlFIN
)

func udpSecurity(key []byte) *dtls.Config {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(udpProtocol))
	psk := m.Sum(nil)
	return &dtls.Config{
		PSK: func(hint []byte) ([]byte, error) {
			if !hmac.Equal(hint, []byte(udpProtocol)) {
				return nil, errors.New("invalid UDP VPN identity")
			}
			return append([]byte(nil), psk...), nil
		},
		PSKIdentityHint:        []byte(udpProtocol),
		CipherSuites:           []dtls.CipherSuiteID{dtls.TLS_PSK_WITH_AES_128_GCM_SHA256},
		ExtendedMasterSecret:   dtls.RequireExtendedMasterSecret,
		SupportedProtocols:     []string{udpProtocol},
		ReplayProtectionWindow: 4096,
		MTU:                    udpFrameMax,
		// Establishment is bounded to 8 seconds: a lost handshake flight fails
		// this attempt instead of scheduling any tunnel-level retransmission.
		FlightInterval: 30 * time.Second,
	}
}

// UDP application records never carry acknowledgements or request retransmits.
// Epochs, per-direction nonces, encryption and replay checks belong to DTLS.
func encodeUDPFrame(kind byte, p []byte) ([]byte, error) {
	if kind < udpControl || kind > udpControlFIN || len(p) > udpFrameMax-udpHeaderSize || (kind >= udpKeepalive && len(p) != 0) {
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
	if len(b) < udpHeaderSize || len(b) > udpFrameMax || string(b[:4]) != "SGU\x01" || b[5] != 0 || int(binary.BigEndian.Uint16(b[6:8])) != len(b)-udpHeaderSize || b[4] < udpControl || b[4] > udpControlFIN {
		return 0, nil, errors.New("invalid UDP VPN frame")
	}
	if b[4] >= udpKeepalive && len(b) != udpHeaderSize {
		return 0, nil, errors.New("invalid UDP VPN command payload")
	}
	return b[4], b[8:], nil
}

type udpConnection struct {
	raw            net.Conn
	ctx            context.Context
	cancel         context.CancelFunc
	reader, writer net.Conn
	control        *udpControlChannel
	packets        chan []byte
	stats          *Stats
	writeMu        sync.Mutex
	writeDeadline  time.Time
	once           sync.Once
}

func newUDPConnection(raw net.Conn, stats *Stats) *udpConnection {
	ctx, cancel := context.WithCancel(context.Background())
	r, w := net.Pipe()
	c := &udpConnection{raw: raw, ctx: ctx, cancel: cancel, reader: r, writer: w, packets: make(chan []byte, queueSize), stats: stats}
	c.control = &udpControlChannel{c: c}
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
			if _, err = c.writer.Write(p); err != nil {
				return
			}
		case udpControlFIN:
			c.writer.Close()
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
	raw, err := dtls.Dial("udp", address, udpSecurity(c.Key))
	if err != nil {
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
