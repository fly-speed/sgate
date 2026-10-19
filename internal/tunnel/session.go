package tunnel

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
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
	chunkSize   = 16 * 1024
	frameHeader = 25
)

var errSessionClosed = errors.New("session closed")

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
	f := frame{op: p[0], seq: binary.BigEndian.Uint64(p[1:]), ack: binary.BigEndian.Uint64(p[9:]), stream: binary.BigEndian.Uint64(p[17:]), data: p[frameHeader:]}
	if f.op > opReset || (f.op == opAck && (f.seq != 0 || f.stream != 0 || len(f.data) != 0)) || (f.op != opAck && (f.seq == 0 || f.stream == 0)) || (f.op != opData && len(f.data) != 0) || (f.op == opData && len(f.data) == 0) {
		return frame{}, errors.New("invalid frame fields")
	}
	return f, nil
}

type stream struct {
	conn                  net.Conn
	readEnded, writeEnded bool
}

type session struct {
	ctx                                    context.Context
	cancel                                 context.CancelFunc
	target                                 string
	opts                                   Options
	mu                                     sync.Mutex
	changed                                chan struct{}
	resets                                 chan uint64
	pending                                []frame
	pendingBytes                           int
	next, received, nextStream, lastOpened uint64
	streams                                map[uint64]*stream
	active                                 *websocket.Conn
	closed                                 bool
	transportMu                            sync.Mutex
}

func newSession(parent context.Context, target string, opts Options) *session {
	ctx, cancel := context.WithCancel(parent)
	s := &session{ctx: ctx, cancel: cancel, target: target, opts: opts, changed: make(chan struct{}), resets: make(chan uint64, opts.MaxStreams), streams: make(map[uint64]*stream)}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case id := <-s.resets:
				if s.enqueue(opReset, id, nil) != nil {
					return
				}
			}
		}
	}()
	return s
}
func (s *session) signalLocked() { close(s.changed); s.changed = make(chan struct{}) }
func (s *session) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	s.cancel()
	if s.active != nil {
		s.active.Close()
	}
	for _, st := range s.streams {
		st.conn.Close()
	}
	s.streams = make(map[uint64]*stream)
	s.pending = nil
	s.pendingBytes = 0
	s.signalLocked()
}
func (s *session) enqueue(op byte, id uint64, data []byte) error {
	cost := len(data) + frameHeader
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return errSessionClosed
		}
		if s.pendingBytes+cost <= s.opts.BufferBytes {
			s.next++
			s.pending = append(s.pending, frame{op: op, seq: s.next, stream: id, data: append([]byte(nil), data...)})
			s.pendingBytes += cost
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
func (s *session) drop(id uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.streams[id]; st != nil {
		st.conn.Close()
		delete(s.streams, id)
	}
}
func (s *session) reset(id uint64) {
	s.drop(id)
	// A full retransmission buffer must never block the transport reader's ACK processing.
	select {
	case s.resets <- id:
	case <-s.ctx.Done():
	default:
		// Bound control work as well as data, even for a misbehaving authenticated peer.
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
func (s *session) readStream(id uint64, c net.Conn) {
	buf := make([]byte, chunkSize)
	for {
		n, err := c.Read(buf)
		if n > 0 {
			if s.enqueue(opData, id, buf[:n]) != nil {
				return
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				_ = s.enqueue(opFin, id, nil)
				s.halfDone(id, true)
			} else {
				s.mu.Lock()
				exists := s.streams[id] != nil
				s.mu.Unlock()
				if exists {
					s.reset(id)
				}
			}
			return
		}
	}
}
func (s *session) addLocal(c net.Conn) error {
	s.mu.Lock()
	if s.closed || len(s.streams) >= s.opts.MaxStreams {
		s.mu.Unlock()
		c.Close()
		return errors.New("session closed or stream limit reached")
	}
	s.nextStream++
	id := s.nextStream
	s.streams[id] = &stream{conn: c}
	s.mu.Unlock()
	if err := s.enqueue(opOpen, id, nil); err != nil {
		s.drop(id)
		return err
	}
	go s.readStream(id, c)
	return nil
}
func (s *session) apply(f frame) error {
	if f.op == opOpen {
		if s.target == "" {
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
			s.reset(f.stream)
			return nil
		}
		c, err := (&net.Dialer{Timeout: s.opts.DialTimeout, KeepAlive: 30 * time.Second}).DialContext(s.ctx, "tcp", s.target)
		if err != nil {
			s.reset(f.stream)
			return nil
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			c.Close()
			return errSessionClosed
		}
		s.streams[f.stream] = &stream{conn: c}
		s.mu.Unlock()
		go s.readStream(f.stream, c)
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
		_ = st.conn.SetWriteDeadline(time.Now().Add(s.opts.WriteTimeout))
		data := f.data
		for len(data) > 0 {
			n, err := st.conn.Write(data)
			if err != nil || n == 0 {
				s.reset(f.stream)
				return nil
			}
			data = data[n:]
		}
		_ = st.conn.SetWriteDeadline(time.Time{})
	case opFin:
		if c, ok := st.conn.(interface{ CloseWrite() error }); ok {
			_ = c.CloseWrite()
		} else {
			s.reset(f.stream)
			return nil
		}
		s.halfDone(f.stream, false)
	case opReset:
		s.drop(f.stream)
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
		s.pending = s.pending[n:]
		s.signalLocked()
	}
	return nil
}

// transport serializes replacement connections so only one reader applies frames.
func (s *session) transport(ws *websocket.Conn, crypt *codec) error {
	s.transportMu.Lock()
	defer s.transportMu.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		ws.Close()
		return errSessionClosed
	}
	s.active = ws
	s.signalLocked()
	s.mu.Unlock()
	defer func() {
		ws.Close()
		s.mu.Lock()
		s.active = nil
		s.signalLocked()
		s.mu.Unlock()
	}()
	ws.SetReadLimit(frameHeader + chunkSize + 24)
	_ = ws.SetReadDeadline(time.Now().Add(s.opts.Heartbeat * 3))
	ws.SetPongHandler(func(string) error { return ws.SetReadDeadline(time.Now().Add(s.opts.Heartbeat * 3)) })
	done := make(chan struct{})
	writerDone := make(chan error, 1)
	go func() { err := s.writeTransport(ws, crypt, done); ws.Close(); writerDone <- err }()
	err := func() error {
		for {
			typ, p, err := ws.ReadMessage()
			if err != nil {
				return err
			}
			if typ != websocket.BinaryMessage {
				return errors.New("expected binary WebSocket message")
			}
			_ = ws.SetReadDeadline(time.Now().Add(s.opts.Heartbeat * 3))
			p, err = crypt.open(p)
			if err != nil {
				return fmt.Errorf("decrypt: %w", err)
			}
			f, err := decodeFrame(p)
			if err != nil {
				return err
			}
			if err = s.acknowledge(f.ack); err != nil {
				return err
			}
			if f.op == opAck {
				continue
			}
			s.mu.Lock()
			received := s.received
			s.mu.Unlock()
			if f.seq > received+1 {
				return errors.New("gap in received sequence")
			}
			if f.seq == received+1 {
				if err = s.apply(f); err != nil {
					return err
				}
				s.mu.Lock()
				s.received = f.seq
				s.signalLocked()
				s.mu.Unlock()
			} else {
				s.mu.Lock()
				s.signalLocked()
				s.mu.Unlock()
			}
		}
	}()
	close(done)
	ws.Close()
	<-writerDone
	return err
}
func (s *session) writeTransport(ws *websocket.Conn, crypt *codec, done <-chan struct{}) error {
	ticker := time.NewTicker(s.opts.Heartbeat)
	defer ticker.Stop()
	var sent uint64
	lastAck := ^uint64(0) // Send a resume ACK even when neither side has pending data.
	for {
		s.mu.Lock()
		ack := s.received
		var next frame
		found := false
		for _, f := range s.pending {
			if f.seq > sent {
				next = f
				found = true
				break
			}
		}
		ch := s.changed
		closed := s.closed
		s.mu.Unlock()
		if closed {
			return errSessionClosed
		}
		if found || ack != lastAck {
			if !found {
				next = frame{op: opAck}
			}
			next.ack = ack
			_ = ws.SetWriteDeadline(time.Now().Add(s.opts.WriteTimeout))
			if err := ws.WriteMessage(websocket.BinaryMessage, crypt.seal(next.encode())); err != nil {
				return err
			}
			if found {
				sent = next.seq
			}
			lastAck = ack
			// Check cancellation even when a producer keeps the queue busy.
			select {
			case <-done:
				return nil
			case <-s.ctx.Done():
				return s.ctx.Err()
			default:
			}
			continue
		}
		select {
		case <-done:
			return nil
		case <-s.ctx.Done():
			return s.ctx.Err()
		case <-ch:
		case <-ticker.C:
			if err := ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(s.opts.WriteTimeout)); err != nil {
				return err
			}
		}
	}
}
