package vpn

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/quic-go/quic-go"
)

// The authenticated VPN protocol uses one control channel and packet records.
// QUIC maps packets to DATAGRAM fragments; TCP/WSS use reliable whole-IP records.
type controlChannel interface {
	io.ReadWriteCloser
	SetDeadline(time.Time) error
	SetReadDeadline(time.Time) error
	CancelRead(quic.StreamErrorCode)
}
type connection interface {
	Context() context.Context
	RemoteAddr() net.Addr
	OpenStreamSync(context.Context) (controlChannel, error)
	AcceptStream(context.Context) (controlChannel, error)
	SendDatagram([]byte) error
	ReceiveDatagram(context.Context) ([]byte, error)
	supportsPackets() bool
	CloseWithError(quic.ApplicationErrorCode, string) error
}
type quicConnection struct{ *quic.Conn }

func (c quicConnection) supportsPackets() bool { return c.Conn.ConnectionState().SupportsDatagrams }

func (c quicConnection) OpenStreamSync(ctx context.Context) (controlChannel, error) {
	return c.Conn.OpenStreamSync(ctx)
}
func (c quicConnection) AcceptStream(ctx context.Context) (controlChannel, error) {
	return c.Conn.AcceptStream(ctx)
}

func dialConnection(ctx context.Context, c Config) (connection, error) {
	if c.Transport == "udp" {
		return dialUDP(ctx, c)
	}
	if c.Transport == "" || c.Transport == "quic" {
		q, err := quic.DialAddr(ctx, c.Address, c.TLS, quicConfig(c.Stats))
		if err != nil {
			return nil, err
		}
		return quicConnection{q}, nil
	}
	tlsConfig := c.TLS.Clone()
	tlsConfig.NextProtos = []string{"http/1.1"}
	if c.Transport == "wss" {
		d := websocket.Dialer{TLSClientConfig: tlsConfig, Subprotocols: []string{Protocol}, HandshakeTimeout: 10 * time.Second, Proxy: nil, NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", c.Address)
		}}
		host := c.Host
		if host == "" {
			host = c.Address
		}
		ws, response, err := d.DialContext(ctx, "wss://"+host+"/vpn", nil)
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		if err != nil {
			return nil, err
		}
		if ws.Subprotocol() != Protocol {
			ws.Close()
			return nil, errors.New("VPN WSS subprotocol was not negotiated")
		}
		ws.SetReadLimit(8192)
		return newRecordConnection(&websocketConn{Conn: ws}), nil
	}
	raw, err := (&tls.Dialer{Config: tlsConfig}).DialContext(ctx, "tcp", c.Address)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			raw.Close()
		}
	}()
	deadline := time.Now().Add(10 * time.Second)
	if d, has := ctx.Deadline(); has && d.Before(deadline) {
		deadline = d
	}
	raw.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { raw.Close() })
	defer stop()
	host := c.Host
	if host == "" {
		host = c.Address
	}
	request, err := http.NewRequest("GET", "https://"+host+"/vpn", nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", Protocol)
	if err = request.Write(raw); err != nil {
		return nil, err
	}
	reader := bufio.NewReader(raw)
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusSwitchingProtocols || response.Header.Get("Upgrade") != Protocol {
		response.Body.Close()
		return nil, fmt.Errorf("VPN TCP upgrade rejected: HTTP %d", response.StatusCode)
	}
	raw.SetDeadline(time.Time{})
	ok = true
	return newRecordConnection(&bufferedConnection{Conn: raw, reader: reader}), nil
}

type bufferedConnection struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConnection) Read(p []byte) (int, error) { return c.reader.Read(p) }

type websocketConn struct {
	*websocket.Conn
	reader        io.Reader
	writeDeadline atomic.Int64
}

func (c *websocketConn) Read(p []byte) (int, error) {
	for {
		if c.reader == nil {
			kind, r, e := c.NextReader()
			if e != nil {
				return 0, e
			}
			if kind != websocket.BinaryMessage {
				return 0, errors.New("VPN WSS requires binary messages")
			}
			c.reader = r
		}
		n, e := c.reader.Read(p)
		if e == io.EOF {
			c.reader = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, e
	}
}
func (c *websocketConn) Write(p []byte) (int, error) {
	deadline := time.Time{}
	if n := c.writeDeadline.Load(); n != 0 {
		deadline = time.Unix(0, n)
	}
	c.Conn.SetWriteDeadline(deadline)
	if e := c.WriteMessage(websocket.BinaryMessage, p); e != nil {
		return 0, e
	}
	return len(p), nil
}
func (c *websocketConn) SetDeadline(t time.Time) error {
	if e := c.SetReadDeadline(t); e != nil {
		return e
	}
	return c.SetWriteDeadline(t)
}
func (c *websocketConn) SetWriteDeadline(t time.Time) error {
	n := int64(0)
	if !t.IsZero() {
		n = t.UnixNano()
	}
	c.writeDeadline.Store(n)
	return c.UnderlyingConn().SetWriteDeadline(t)
}
func (c *websocketConn) LocalAddr() net.Addr  { return c.UnderlyingConn().LocalAddr() }
func (c *websocketConn) RemoteAddr() net.Addr { return c.UnderlyingConn().RemoteAddr() }

const recordControl = 1
const recordPacket = 2
const recordFIN = 3
const recordClose = 4
const recordPing = 5
const recordIP = 6

type recordConnection struct {
	raw            net.Conn
	ctx            context.Context
	cancel         context.CancelFunc
	reader, writer net.Conn
	control        *recordControlChannel
	packets        chan []byte
	writeMu        sync.Mutex
	once           sync.Once
	errMu          sync.Mutex
	err            error
	done           chan struct{}
	pingDone       chan struct{}
	closing        atomic.Bool
}

func newRecordConnection(raw net.Conn) *recordConnection {
	ctx, cancel := context.WithCancel(context.Background())
	r, w := net.Pipe()
	c := &recordConnection{raw: raw, ctx: ctx, cancel: cancel, reader: r, writer: w, packets: make(chan []byte, 64), done: make(chan struct{}), pingDone: make(chan struct{})}
	c.control = &recordControlChannel{c: c}
	go c.readRecords()
	go c.keepAlive()
	return c
}
func (c *recordConnection) Context() context.Context { return c.ctx }
func (c *recordConnection) RemoteAddr() net.Addr     { return c.raw.RemoteAddr() }
func (c *recordConnection) supportsPackets() bool {
	return true
}
func (c *recordConnection) OpenStreamSync(ctx context.Context) (controlChannel, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.control, nil
}
func (c *recordConnection) AcceptStream(ctx context.Context) (controlChannel, error) {
	return c.OpenStreamSync(ctx)
}
func (c *recordConnection) failure() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	if c.err != nil {
		return c.err
	}
	return io.EOF
}
func (c *recordConnection) finish(err error) {
	c.once.Do(func() {
		c.errMu.Lock()
		c.err = err
		c.errMu.Unlock()
		c.cancel()
		c.raw.Close()
		c.reader.Close()
		c.writer.Close()
	})
}
func (c *recordConnection) write(kind byte, p []byte) error {
	if len(p) > 8192 {
		return errors.New("VPN record too large")
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.ctx.Err() != nil {
		return c.failure()
	}
	data := make([]byte, 3+len(p))
	data[0] = kind
	binary.BigEndian.PutUint16(data[1:3], uint16(len(p)))
	copy(data[3:], p)
	for len(data) > 0 {
		n, err := c.raw.Write(data)
		if err != nil {
			c.finish(err)
			return err
		}
		if n == 0 {
			c.finish(io.ErrNoProgress)
			return io.ErrNoProgress
		}
		data = data[n:]
	}
	return nil
}
func (c *recordConnection) SendDatagram(p []byte) error { return c.write(recordPacket, p) }

// Reliable transports carry one whole IP packet, avoiding UDP fragment expiry
// during TCP loss recovery. The peer validates IP addresses and the configured MTU.
func (c *recordConnection) sendIP(p []byte) error { return c.write(recordIP, p) }
func (c *recordConnection) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	select {
	case p := <-c.packets:
		return p, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.ctx.Done():
		return nil, c.failure()
	}
}
func (c *recordConnection) CloseWithError(code quic.ApplicationErrorCode, reason string) error {
	c.closing.Store(true)
	timer := time.AfterFunc(100*time.Millisecond, func() { c.finish(io.EOF) })
	defer timer.Stop()
	// Bound a close behind any blocked data write; shutdown must release workers.
	c.raw.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
	p := make([]byte, 8)
	binary.BigEndian.PutUint64(p, uint64(code))
	if len(reason) > 256 {
		reason = reason[:256]
	}
	p = append(p, reason...)
	c.write(recordClose, p)
	c.finish(io.EOF)
	<-c.done
	<-c.pingDone
	return nil
}
func (c *recordConnection) readRecords() {
	defer close(c.done)
	defer c.finish(io.EOF)
	for {
		var h [3]byte
		if _, err := io.ReadFull(c.raw, h[:]); err != nil {
			c.finish(err)
			return
		}
		n := int(binary.BigEndian.Uint16(h[1:]))
		if n > 8192 || (h[0] == recordPacket && (n == 0 || n > fragmentPayload+headerSize)) || (h[0] == recordControl && n > 4100) || (h[0] == recordIP && (n < 20 || n > 1500)) {
			c.finish(errors.New("invalid VPN record length"))
			return
		}
		p := make([]byte, n)
		if _, err := io.ReadFull(c.raw, p); err != nil {
			c.finish(err)
			return
		}
		switch h[0] {
		case recordControl:
			if _, err := c.writer.Write(p); err != nil {
				return
			}
		case recordIP:
			select {
			case c.packets <- append([]byte{2, 0}, p...):
			case <-c.ctx.Done():
				return
			}
		case recordPacket:
			select {
			case c.packets <- p:
			case <-c.ctx.Done():
				return
			}
		case recordPing:
			if n != 0 {
				return
			}
		case recordFIN:
			if n != 0 {
				return
			}
			c.writer.Close()
		case recordClose:
			if n < 8 || n > 264 {
				return
			}
			c.finish(&quic.ApplicationError{Remote: true, ErrorCode: quic.ApplicationErrorCode(binary.BigEndian.Uint64(p[:8])), ErrorMessage: string(p[8:])})
			return
		default:
			c.finish(errors.New("invalid VPN record type"))
			return
		}
	}
}

type recordControlChannel struct{ c *recordConnection }

func (s *recordControlChannel) Read(p []byte) (int, error) {
	n, err := s.c.reader.Read(p)
	if err != nil && s.c.ctx.Err() != nil {
		return n, s.c.failure()
	}
	return n, err
}
func (s *recordControlChannel) Write(p []byte) (int, error) {
	if err := s.c.write(recordControl, p); err != nil {
		return 0, err
	}
	return len(p), nil
}
func (s *recordControlChannel) Close() error { return s.c.write(recordFIN, nil) }
func (s *recordControlChannel) SetReadDeadline(t time.Time) error {
	return s.c.reader.SetReadDeadline(t)
}
func (s *recordControlChannel) SetDeadline(t time.Time) error {
	if err := s.SetReadDeadline(t); err != nil {
		return err
	}
	if s.c.closing.Load() {
		return io.EOF
	}
	return s.c.raw.SetWriteDeadline(t)
}
func (s *recordControlChannel) CancelRead(quic.StreamErrorCode) { s.c.reader.Close() }

func (c *recordConnection) keepAlive() {
	defer close(c.pingDone)
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			if c.write(recordPing, nil) != nil {
				return
			}
		}
	}
}
