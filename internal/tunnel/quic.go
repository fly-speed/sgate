package tunnel

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

// QUIC carries the existing authenticated handshake and tunnel framing over a
// reliable UDP stream. It is a private protocol, not HTTP/3 or WebTransport.
const quicProtocol = "sgate-quic-v1"

func quicConfig(o Options) *quic.Config {
	return &quic.Config{
		HandshakeIdleTimeout:       o.DialTimeout,
		MaxIdleTimeout:             o.HeartbeatTimeout,
		KeepAlivePeriod:            o.Heartbeat,
		MaxIncomingStreams:         1,
		MaxIncomingUniStreams:      -1,
		MaxStreamReceiveWindow:     uint64(o.BufferBytes),
		MaxConnectionReceiveWindow: uint64(o.BufferBytes),
	}
}

func quicTLS(config *tls.Config) *tls.Config {
	if config == nil {
		config = &tls.Config{}
	}
	result := config.Clone()
	result.MinVersion = tls.VersionTLS13
	result.NextProtos = []string{quicProtocol}
	// Preserve certificate rotation without allowing a returned configuration to
	// replace this protocol's ALPN or TLS minimum.
	if get := result.GetConfigForClient; get != nil {
		result.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			next, err := get(hello)
			if err != nil || next == nil {
				return next, err
			}
			return quicTLS(next), nil
		}
	}
	return result
}

// ListenQUIC adapts one bidirectional stream per QUIC connection to net.Listener.
// Closing it closes the UDP socket and all accepted QUIC connections.
// Callers also close tunnel sessions to release their business sockets.
func ListenQUIC(address string, config *tls.Config, options Options) (net.Listener, error) {
	if config == nil || (len(config.Certificates) == 0 && config.GetCertificate == nil && config.GetConfigForClient == nil) {
		return nil, errors.New("QUIC requires a TLS certificate and key")
	}
	o, err := options.defaults()
	if err != nil {
		return nil, err
	}
	packet, err := net.ListenPacket("udp", address)
	if err != nil {
		return nil, err
	}
	transport := &quic.Transport{Conn: packet}
	listener, err := transport.Listen(quicTLS(config), quicConfig(o))
	if err != nil {
		transport.Close()
		packet.Close()
		return nil, err
	}
	return &quicListener{listener: listener, transport: transport, packet: packet, timeout: o.DialTimeout, linger: o.WriteTimeout}, nil
}

type quicListener struct {
	transport *quic.Transport
	packet    net.PacketConn
	closeOnce sync.Once
	listener  *quic.Listener
	timeout   time.Duration
	linger    time.Duration
}

func (l *quicListener) Addr() net.Addr { return l.listener.Addr() }
func (l *quicListener) Close() error {
	l.closeOnce.Do(func() { _ = l.listener.Close(); _ = l.transport.Close(); _ = l.packet.Close() })
	return nil
}
func (l *quicListener) Accept() (net.Conn, error) {
	c, err := l.listener.Accept(context.Background())
	if err != nil {
		return nil, err
	}
	// AcceptStream is deferred to the HTTP connection goroutine. A peer that
	// completes TLS but never opens a stream must not block other admissions.
	return &quicStreamConn{conn: c, timeout: l.timeout, linger: l.linger}, nil
}

type quicStreamConn struct {
	stateMu   sync.Mutex
	writeMu   sync.Mutex
	closeOnce sync.Once
	linger    time.Duration
	conn      *quic.Conn
	stream    *quic.Stream
	once      sync.Once
	err       error
	timeout   time.Duration
}

func (c *quicStreamConn) init() error {
	c.once.Do(func() {
		if c.stream != nil {
			return
		}
		ctx, cancel := context.WithTimeout(c.conn.Context(), c.timeout)
		defer cancel()
		stream, err := c.conn.AcceptStream(ctx)
		c.stateMu.Lock()
		c.stream, c.err = stream, err
		c.stateMu.Unlock()
		if c.err != nil {
			_ = c.conn.CloseWithError(0, "stream unavailable")
		}
	})
	return c.err
}
func (c *quicStreamConn) Read(p []byte) (int, error) {
	if err := c.init(); err != nil {
		return 0, err
	}
	return c.stream.Read(p)
}
func (c *quicStreamConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.init(); err != nil {
		return 0, err
	}
	return c.stream.Write(p)
}
func (c *quicStreamConn) Close() error {
	c.closeOnce.Do(func() {
		c.stateMu.Lock()
		stream := c.stream
		c.stateMu.Unlock()
		if stream == nil {
			_ = c.conn.CloseWithError(0, "closed")
			return
		}
		// Connection close discards queued bytes. FIN allows retransmission of the
		// final probe response and authenticated close proof during a bounded linger.
		stream.CancelRead(0)
		_ = stream.SetWriteDeadline(time.Now())
		c.writeMu.Lock()
		_ = stream.Close()
		c.writeMu.Unlock()
		go func() {
			timer := time.NewTimer(c.linger)
			defer timer.Stop()
			select {
			case <-c.conn.Context().Done():
			case <-timer.C:
			}
			_ = c.conn.CloseWithError(0, "closed")
		}()
	})
	return nil
}
func (c *quicStreamConn) LocalAddr() net.Addr  { return c.conn.LocalAddr() }
func (c *quicStreamConn) RemoteAddr() net.Addr { return c.conn.RemoteAddr() }
func (c *quicStreamConn) SetDeadline(t time.Time) error {
	if err := c.init(); err != nil {
		return err
	}
	return c.stream.SetDeadline(t)
}
func (c *quicStreamConn) SetReadDeadline(t time.Time) error {
	if err := c.init(); err != nil {
		return err
	}
	return c.stream.SetReadDeadline(t)
}
func (c *quicStreamConn) SetWriteDeadline(t time.Time) error {
	if err := c.init(); err != nil {
		return err
	}
	return c.stream.SetWriteDeadline(t)
}

func (c *Client) dialQUIC(ctx context.Context, address, hostname string) (net.Conn, error) {
	config := quicTLS(c.cfg.TLSConfig)
	if config.ServerName == "" {
		config.ServerName = hostname
	}
	conn, err := quic.DialAddr(ctx, address, config, quicConfig(c.cfg.Options))
	if err != nil {
		return nil, err
	}
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		_ = conn.CloseWithError(0, "stream unavailable")
		return nil, err
	}
	var transport net.Conn = &quicStreamConn{conn: conn, stream: stream, linger: c.cfg.Options.WriteTimeout}
	if stats := c.cfg.Options.Stats; stats != nil {
		transport = stats.Conn(transport)
	}
	return transport, nil
}
