package tunnel

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	opAck byte = iota
	opOpen
	opData
	opFin
	opReset
	opNoise
	opReverseOpen
	opReverseOpenTLS
	chunkSize   = 16 * 1024
	frameHeader = 25
)

var errSessionClosed = errors.New("session closed")
var errStreamClosed = errors.New("stream closed")

type frame struct {
	op               byte
	seq, ack, stream uint64
	data             []byte
}

func (f frame) encode() []byte {
	p := make([]byte, frameHeader+len(f.data))
	p[0] = f.op
	binary.BigEndian.PutUint64(p[1:], f.seq)
	binary.BigEndian.PutUint64(p[9:], f.ack)
	binary.BigEndian.PutUint64(p[17:], f.stream)
	copy(p[frameHeader:], f.data)
	return p
}
func decodeFrame(p []byte) (frame, error) {
	if len(p) < frameHeader || len(p) > frameHeader+chunkSize {
		return frame{}, errors.New("invalid frame length")
	}
	f := frame{
		op:     p[0],
		seq:    binary.BigEndian.Uint64(p[1:]),
		ack:    binary.BigEndian.Uint64(p[9:]),
		stream: binary.BigEndian.Uint64(p[17:]),
		data:   p[frameHeader:],
	}
	if f.op == opNoise {
		if f.seq != 0 || f.ack != 0 || f.stream != 0 || len(f.data) == 0 {
			return frame{}, errors.New("invalid noise frame fields")
		}
		return f, nil
	}
	if (f.op > opReset && f.op != opReverseOpen && f.op != opReverseOpenTLS) || (f.op == opAck && (f.seq != 0 || f.stream != 0 || len(f.data) != 0)) ||
		(f.op != opAck && (f.seq == 0 || f.stream == 0)) ||
		(f.op != opData && f.op != opReset && f.op != opReverseOpen && f.op != opReverseOpenTLS && len(f.data) != 0) ||
		(f.op == opReset && !validResetReason(f.data)) ||
		((f.op == opReverseOpen || f.op == opReverseOpenTLS) && (f.stream <= reverseStreamBase || validateSelectableTarget(string(f.data)) != nil)) ||
		(f.op == opData && len(f.data) == 0) {
		return frame{}, errors.New("invalid frame fields")
	}
	return f, nil
}

type stream struct {
	conn                  net.Conn
	readEnded, writeEnded bool
}

// session 保存跨 WebSocket 重连保留的状态，不能与单次传输的生命周期混用。
// mu 保护序列号、业务流和重放缓存；transportMu 保证同一会话只有一个传输读者。
type session struct {
	lastBusiness                           time.Time // Protected by mu; monotonic application activity, not control traffic.
	ctx                                    context.Context
	cancel                                 context.CancelFunc
	target                                 string
	opts                                   Options
	mu                                     sync.Mutex
	changed                                chan struct{}
	incoming                               []frame
	incomingBytes                          int
	accepted                               uint64
	peerWindow                             int // 0 表示旧版未协商；非零时限制未确认的线帧总量。
	resets                                 chan streamReset
	pending                                []frame
	pendingBytes                           int
	next, received, nextStream, lastOpened uint64
	streams                                map[uint64]*stream
	active                                 *websocket.Conn
	activeCloseProof                       string
	closed                                 bool
	transportMu                            sync.Mutex
	noiseSent, noiseReceived               uint64 // Protected by mu; transport-only diagnostics.
}

func newSession(parent context.Context, target string, opts Options) *session {
	ctx, cancel := context.WithCancel(parent)
	s := &session{
		ctx:     ctx,
		cancel:  cancel,
		target:  target,
		opts:    opts,
		changed: make(chan struct{}),
		resets:  make(chan streamReset, opts.MaxStreams),
		streams: make(map[uint64]*stream),
	}
	if opts.Stats != nil {
		opts.Stats.registerSession(s)
	}
	go s.deliverIncoming()
	go func() {
		defer s.close()
		for {
			select {
			case <-ctx.Done():
				return
			case reset := <-s.resets:
				var reason []byte
				if s.opts.reportStreamErrors && reset.reason != 0 {
					reason = []byte{reset.reason}
				}
				if s.enqueue(opReset, reset.id, reason) != nil {
					return
				}
			}
		}
	}()
	return s
}
func (s *session) signalLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}
func (s *session) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	// Notify before canceling the writer. The proof is specific to this
	// authenticated transport; ordinary/forged Close frames keep retention.
	if s.active != nil && s.activeCloseProof != "" && s.ctx.Err() == nil {
		_ = s.active.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, sessionClosePrefix+s.activeCloseProof), time.Now().Add(100*time.Millisecond))
	}
	s.closed = true
	if s.opts.Stats != nil {
		s.opts.Stats.unregisterSession(s)
	}
	s.cancel()
	if s.active != nil {
		// TLS Close can wait for an in-flight writer before closing the socket.
		// Proxy replacement must unblock that writer without closing the shared
		// management listener or waiting for a Web configuration request itself.
		if tlsConn, ok := s.active.NetConn().(interface{ NetConn() net.Conn }); ok {
			_ = tlsConn.NetConn().Close()
		}
		s.active.Close()
	}
	for _, st := range s.streams {
		st.conn.Close()
	}
	s.streams = make(map[uint64]*stream)
	s.pending = nil
	s.pendingBytes = 0
	s.incoming = nil
	s.incomingBytes = 0
	s.signalLocked()
}
func (s *session) enqueue(op byte, id uint64, data []byte) error {
	return s.enqueueStream(op, id, data, nil)
}

// A producer belongs to a particular stream. Resetting that stream must wake
// blocked producers without waiting for unrelated traffic to be acknowledged.
func (s *session) enqueueStream(op byte, id uint64, data []byte, st *stream) error {
	cost := len(data) + frameHeader
	for {
		s.mu.Lock()
		if s.closed || s.ctx.Err() != nil {
			s.mu.Unlock()
			return errSessionClosed
		}
		if st != nil && s.streams[id] != st {
			s.mu.Unlock()
			return errStreamClosed
		}
		if s.pendingBytes+cost <= s.opts.BufferBytes {
			s.next++
			s.pending = append(s.pending, frame{op: op, seq: s.next, stream: id, data: append([]byte(nil), data...)})
			s.pendingBytes += cost
			if op == opData {
				s.lastBusiness = time.Now()
			}
			s.signalLocked()
			s.mu.Unlock()
			return nil
		}
		ch := s.changed
		s.mu.Unlock()
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		case <-ch:
		}
	}
}
func (s *session) drop(id uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.streams[id]; st != nil {
		st.conn.Close()
		delete(s.streams, id)
		s.signalLocked()
		return true
	}
	return false
}
func (s *session) reset(id uint64) {
	// Reader and writer failures can race. Only the caller that removes the
	// live stream may queue a RESET; duplicates must not exhaust control work.
	if !s.drop(id) {
		return
	}
	s.enqueueReset(id)
}

func (s *session) enqueueReset(id uint64) {
	s.enqueueResetReason(id, 0)
}

func (s *session) enqueueResetReason(id uint64, reason byte) {
	// A full retransmission buffer must never block the transport reader's ACK processing.
	select {
	case s.resets <- streamReset{id: id, reason: reason}:
	case <-s.ctx.Done():
	default:
		// Bound control work as well as data, even for a misbehaving authenticated peer.
		s.opts.Logf("session closed: reset control queue limit reached")
		s.close()
	}
}
func (s *session) halfDone(id uint64, read bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.streams[id]
	if st == nil {
		return
	}
	if read {
		st.readEnded = true
	} else {
		st.writeEnded = true
	}
	if st.readEnded && st.writeEnded {
		st.conn.Close()
		delete(s.streams, id)
	}
}
func (s *session) readStream(id uint64, st *stream) {
	c := st.conn
	if secure, ok := c.(interface{ HandshakeContext(context.Context) error }); ok {
		ctx, cancel := context.WithTimeout(s.ctx, s.opts.DialTimeout)
		err := secure.HandshakeContext(ctx)
		cancel()
		if err != nil {
			s.failStream(id, "TLS handshake", err, resetDialFailed)
			return
		}
		_ = c.SetDeadline(time.Time{})
	}
	buf := make([]byte, chunkSize)
	for {
		n, err := c.Read(buf)
		// close 在 mu 内取消会话并关闭全部连接。已取消的读取协程直接退出，
		// 避免同时等待 mu 来检查已经失效的流表；隧道重连不会取消会话。
		if s.ctx.Err() != nil {
			return
		}
		if n > 0 {
			if s.enqueueStream(opData, id, buf[:n], st) != nil {
				return
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if s.enqueueStream(opFin, id, nil, st) != nil {
					return
				}
				s.halfDone(id, true)
			} else {
				s.failStream(id, "TCP read", err, resetReadFailed)
			}
			return
		}
	}
}
func (s *session) addLocal(c net.Conn) error {
	s.mu.Lock()
	if s.closed || s.ctx.Err() != nil {
		s.mu.Unlock()
		c.Close()
		return errSessionClosed
	}
	if len(s.streams) >= s.opts.MaxStreams {
		s.mu.Unlock()
		c.Close()
		return errors.New("client stream limit reached")
	}
	if s.nextStream >= reverseStreamBase-1 {
		s.mu.Unlock()
		c.Close()
		return errors.New("stream IDs exhausted")
	}
	s.nextStream++
	id := s.nextStream
	st := &stream{conn: c}
	if len(s.streams) == 0 {
		s.lastBusiness = time.Time{}
	}
	s.streams[id] = st
	s.mu.Unlock()
	if err := s.enqueueStream(opOpen, id, nil, st); err != nil {
		s.drop(id)
		return err
	}
	go s.readStream(id, st)
	return nil
}
func (s *session) apply(f frame) error {
	if f.op == opOpen || f.op == opReverseOpen || f.op == opReverseOpenTLS {
		target := s.target
		if f.op == opReverseOpen || f.op == opReverseOpenTLS {
			if !s.opts.receiveReverse || s.target != "" || f.stream <= reverseStreamBase || validateSelectableTarget(string(f.data)) != nil {
				return errors.New("reverse OPEN not permitted")
			}
			target = string(f.data)
		} else if f.stream >= reverseStreamBase {
			return errors.New("invalid forward stream ID")
		}
		if target == "" {
			return errors.New("server cannot open client streams")
		}
		s.mu.Lock()
		if f.stream <= s.lastOpened {
			s.mu.Unlock()
			return errors.New("stream ID reused")
		}
		s.lastOpened = f.stream
		full := len(s.streams) >= s.opts.MaxStreams
		s.mu.Unlock()
		if full {
			s.opts.Logf("stream=%d target=%q backend connection rejected: stream limit reached", f.stream, target)
			s.enqueueResetReason(f.stream, resetStreamLimit)
			return nil
		}
		dialer := &net.Dialer{Timeout: s.opts.DialTimeout, KeepAlive: 30 * time.Second}
		var c net.Conn
		var err error
		if f.op == opReverseOpenTLS {
			config := &tls.Config{MinVersion: tls.VersionTLS12}
			if s.opts.reverseTLSConfig != nil {
				config = s.opts.reverseTLSConfig.Clone()
			}
			host, _, _ := net.SplitHostPort(target)
			config.ServerName = host
			config.InsecureSkipVerify = false
			ctx, cancel := context.WithTimeout(s.ctx, s.opts.DialTimeout)
			c, err = (&tls.Dialer{NetDialer: dialer, Config: config}).DialContext(ctx, "tcp", target)
			cancel()
			if err == nil {
				c = &reverseTLSConn{Conn: c.(*tls.Conn)}
			}
		} else {
			c, err = dialer.DialContext(s.ctx, "tcp", target)
		}
		if err != nil {
			if s.ctx.Err() == nil {
				s.opts.Logf("stream=%d target=%q backend connection failed: %v", f.stream, target, err)
			}
			s.enqueueResetReason(f.stream, dialResetReason(err))
			return nil
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			c.Close()
			return errSessionClosed
		}
		// A local admission can race the backend dial in a bidirectional session.
		if len(s.streams) >= s.opts.MaxStreams {
			s.mu.Unlock()
			c.Close()
			s.enqueueResetReason(f.stream, resetStreamLimit)
			return nil
		}
		st := &stream{conn: c}
		if len(s.streams) == 0 {
			s.lastBusiness = time.Time{}
		}
		s.streams[f.stream] = st
		s.mu.Unlock()
		go s.readStream(f.stream, st)
		return nil
	}
	s.mu.Lock()
	st := s.streams[f.stream]
	s.mu.Unlock()
	if st == nil {
		return nil
	} // Data already in flight after a reset is intentionally discarded.
	switch f.op {
	case opData:
		if st.writeEnded {
			return errors.New("data after FIN")
		}
		s.noteBusiness()
		// 业务应用可长时间暂停读取；会话关闭会关闭 TCP 并中断此写入。
		// WriteTimeout 仅约束隧道网络写入，不能把正常业务背压当作断线。
		_ = st.conn.SetWriteDeadline(time.Time{})
		data := f.data
		for len(data) > 0 {
			n, err := st.conn.Write(data)
			if err != nil || n == 0 {
				if err == nil {
					err = io.ErrNoProgress
				}
				s.failStream(f.stream, "TCP write", err, resetWriteFailed)
				return nil
			}
			data = data[n:]
		}
		_ = st.conn.SetWriteDeadline(time.Time{})
	case opFin:
		if c, ok := st.conn.(interface{ CloseWrite() error }); ok {
			if err := c.CloseWrite(); err != nil {
				s.failStream(f.stream, "TCP half-close", err, resetWriteFailed)
				return nil
			}
		} else {
			s.failStream(f.stream, "TCP half-close", errors.New("connection does not support half-close"), resetWriteFailed)
			return nil
		}
		s.halfDone(f.stream, false)
	case opReset:
		if s.drop(f.stream) {
			s.opts.Logf("stream=%d local=%q peer=%q connection rejected/reset by remote: %s",
				f.stream, st.conn.LocalAddr(), st.conn.RemoteAddr(), resetReasonText(f.data))
		}
	}
	return nil
}
func (s *session) acknowledge(ack uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ack > s.next {
		return errors.New("ACK beyond sent sequence")
	}
	n := 0
	for n < len(s.pending) && s.pending[n].seq <= ack {
		s.pendingBytes -= frameHeader + len(s.pending[n].data)
		s.pending[n] = frame{}
		n++
	}
	if n > 0 {
		if n == len(s.pending) {
			s.pending = nil
		} else {
			s.pending = s.pending[n:]
		}
		s.signalLocked()
	}
	return nil
}
