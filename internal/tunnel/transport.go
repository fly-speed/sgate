package tunnel

import (
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// 传输层只管理当前 WebSocket；可靠序列、业务连接和重放缓存由 session 持有。
// transport serializes replacement connections so only one reader applies frames.
func (s *session) transport(ws *websocket.Conn, crypt *codec) error {
	return s.transportConfirmed(ws, crypt, time.Time{}, nil)
}

// A resumed peer must confirm the authenticated channel before its original
// retention deadline. A WebSocket upgrade alone may race a client's timeout.
func (s *session) transportConfirmed(ws *websocket.Conn, crypt *codec, resumeDeadline time.Time, onConfirmed func()) error {
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
	confirm, touch, stopConfirmation := transportConfirmation(ws, resumeDeadline, s.opts.HeartbeatTimeout, onConfirmed)
	defer stopConfirmation()
	ws.SetPongHandler(func(string) error { return touch() })
	pongs := make(chan string, 1)
	ws.SetPingHandler(func(data string) error {
		// Never wait for the data writer here: doing so prevents this reader
		// from consuming ACKs and can deadlock bidirectional backpressure.
		queueLatestPong(pongs, data)
		return touch()
	})
	done := make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		err := s.writeTransport(ws, crypt, done, pongs)
		ws.Close()
		writerDone <- err
	}()
	err := s.readTransport(ws, crypt, touch, confirm)
	close(done)
	ws.Close()
	if writerErr := <-writerDone; writerErr != nil {
		// The writer closes the socket to wake the reader. Preserve its cause
		// instead of reporting only the resulting closed-connection read.
		return errors.Join(err, fmt.Errorf("transport writer: %w", writerErr))
	}
	return err
}

type progressReader struct {
	io.Reader
	touch func() error
}

func (r progressReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if n > 0 {
		if touchErr := r.touch(); err == nil {
			err = touchErr
		}
	}
	return n, err
}

func readTransportMessage(ws *websocket.Conn, touch func() error) ([]byte, error) {
	if err := touch(); err != nil {
		return nil, err
	}
	typ, reader, err := ws.NextReader()
	if err != nil {
		return nil, err
	}
	if typ != websocket.BinaryMessage {
		return nil, errors.New("expected binary WebSocket message")
	}
	return io.ReadAll(progressReader{Reader: reader, touch: touch})
}

// RFC 6455 section 5.5.3 permits replying only to the latest pending Ping.
// There is one producer (the reader); keep at most one immutable payload.
func queueLatestPong(pongs chan string, data string) {
	select {
	case pongs <- data:
		return
	default:
	}
	select {
	case <-pongs:
	default:
	}
	select {
	case pongs <- data:
	default:
	}
}

func (s *session) writeTransport(ws *websocket.Conn, crypt *codec, done <-chan struct{}, pongs <-chan string) error {
	ticker := time.NewTicker(s.opts.Heartbeat)
	defer ticker.Stop()
	var noiseTicks <-chan time.Time
	var noiseTimer *time.Timer
	delay, err := s.opts.noiseDelay()
	if err != nil {
		return err
	}
	if delay > 0 {
		noiseTimer = time.NewTimer(delay)
		defer noiseTimer.Stop()
		noiseTicks = noiseTimer.C
	}
	noiseDue := false
	var sent uint64
	lastAck := ^uint64(0) // Send a resume ACK even when neither side has pending data.
	for {
		select {
		case <-done:
			return nil
		case <-s.ctx.Done():
			return s.ctx.Err()
		default:
		}
		due, err := s.pollControl(ws, pongs, noiseTicks, ticker.C)
		if err != nil {
			return err
		}
		noiseDue = noiseDue || due
		ack, next, found, ch, closed := s.nextOutgoing(sent)
		if closed {
			return errSessionClosed
		}
		if found || ack != lastAck || noiseDue {
			// Give one queued application frame priority, even when padding is due.
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
			}
			if noiseDue {
				if err := s.writeNoise(ws, crypt); err != nil {
					return err
				}
				noiseDue = false
				// Start a fresh wait after sending; never accumulate overdue padding.
				delay, err := s.opts.noiseDelay()
				if err != nil {
					return err
				}
				noiseTimer.Reset(delay)
			}
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
		case data := <-pongs:
			if err := s.writeControl(ws, websocket.PongMessage, []byte(data)); err != nil {
				return err
			}
		case <-noiseTicks:
			noiseDue = true
		case <-ticker.C:
			if err := s.writeControl(ws, websocket.PingMessage, nil); err != nil {
				return err
			}
		}
	}
}

// readTransport 先校验并处理 ACK，再按序交付应用帧；重复帧只唤醒 ACK，不重复写入业务 TCP。
func (s *session) readTransport(ws *websocket.Conn, crypt *codec, touch, confirm func() error) error {
	for {
		// Local TCP work and a large, slowly arriving frame must not consume the
		// next read's idle budget. Refresh on each payload read, not just per message.
		p, err := readTransportMessage(ws, touch)
		if err != nil {
			return err
		}
		p, err = crypt.open(p)
		if err != nil {
			return fmt.Errorf("decrypt: %w", err)
		}
		f, err := decodeFrame(p)
		if err != nil {
			return err
		}
		if f.op == opNoise {
			if err := confirm(); err != nil {
				return err
			}
			// Padding belongs only to this transport, never to reliable streams.
			s.mu.Lock()
			s.noiseReceived++
			s.mu.Unlock()
			continue
		}
		if err = s.acknowledge(f.ack); err != nil {
			return err
		}
		if err := confirm(); err != nil {
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
}

// 控制帧和半帧读取只能更新读空闲时间，不能延长尚未确认重连的原保留期限。
func transportConfirmation(
	ws *websocket.Conn,
	resumeDeadline time.Time,
	heartbeatTimeout time.Duration,
	onConfirmed func(),
) (
	confirm func() error,
	touch func() error,
	stop func(),
) {
	confirmed := false
	// Keep the original resume deadline independent of read/control progress.
	// Expiry closes the socket and also interrupts blocked outbound writes.
	var confirmation atomic.Uint32 // 0 pending, 1 confirmed, 2 expired
	var resumeTimer *time.Timer
	if !resumeDeadline.IsZero() {
		resumeTimer = time.AfterFunc(time.Until(resumeDeadline), func() {
			if confirmation.CompareAndSwap(0, 2) {
				ws.Close()
			}
		})
	}
	confirm = func() error {
		if !confirmed {
			if !resumeDeadline.IsZero() && !time.Now().Before(resumeDeadline) {
				confirmation.CompareAndSwap(0, 2)
			}
			if !confirmation.CompareAndSwap(0, 1) {
				return errors.New("resume confirmation deadline expired")
			}
			if resumeTimer != nil {
				resumeTimer.Stop()
			}
			confirmed = true
			if onConfirmed != nil {
				onConfirmed()
			}
		}
		return nil
	}
	touch = func() error {
		deadline := time.Now().Add(heartbeatTimeout)
		if !confirmed && !resumeDeadline.IsZero() && resumeDeadline.Before(deadline) {
			deadline = resumeDeadline
		}
		return ws.SetReadDeadline(deadline)
	}
	stop = func() {
		if resumeTimer != nil {
			resumeTimer.Stop()
		}
	}
	return confirm, touch, stop
}

func (s *session) nextOutgoing(sent uint64) (ack uint64, next frame, found bool, changed <-chan struct{}, closed bool) {
	s.mu.Lock()
	ack = s.received
	for _, f := range s.pending {
		if f.seq > sent {
			next = f
			found = true
			break
		}
	}
	changed = s.changed
	closed = s.closed
	s.mu.Unlock()
	return
}

// 控制帧始终在数据写协程处理；读协程只入队，避免阻塞 ACK 接收。
func (s *session) pollControl(ws *websocket.Conn, pongs <-chan string, noiseTicks, heartbeat <-chan time.Time) (
	bool,
	error,
) {
	noiseDue := false
	select {
	case data := <-pongs:
		if err := s.writeControl(ws, websocket.PongMessage, []byte(data)); err != nil {
			return false, err
		}
	default:
	}
	select {
	case <-noiseTicks:
		noiseDue = true
	default:
	}
	// A busy data/ACK queue must not starve heartbeat probes.
	select {
	case <-heartbeat:
		if err := s.writeControl(ws, websocket.PingMessage, nil); err != nil {
			return false, err
		}
	default:
	}
	return noiseDue, nil
}

func (s *session) writeControl(ws *websocket.Conn, kind int, data []byte) error {
	return ws.WriteControl(kind, data, time.Now().Add(s.opts.WriteTimeout))
}
