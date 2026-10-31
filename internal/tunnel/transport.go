package tunnel

import (
	"crypto/hmac"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const sessionClosePrefix = "sgate-end:"

var errPeerSessionClosed = errors.New("peer ended session")

// 传输层只管理当前 WebSocket；可靠序列、业务连接和重放缓存由 session 持有。
// transport serializes replacement connections so only one reader applies frames.
func (s *session) transport(ws *websocket.Conn, crypt *codec) error {
	return s.transportConfirmed(ws, crypt, time.Time{}, nil)
}

// A resumed peer must confirm the authenticated channel before its original
// retention deadline. A WebSocket upgrade alone may race a client's timeout.
func (s *session) transportConfirmed(ws *websocket.Conn, crypt *codec, resumeDeadline time.Time, onConfirmed func()) (transportErr error) {
	s.transportMu.Lock()
	defer s.transportMu.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		ws.Close()
		return errSessionClosed
	}
	s.active = ws
	s.activeCloseProof = ""
	if crypt.client && s.opts.closeSessionOnStop {
		s.activeCloseProof = crypt.closeProof
	}
	s.peerWindow = crypt.peerWindow
	s.signalLocked()
	s.mu.Unlock()
	defer func() {
		if s.opts.Stats != nil {
			s.opts.Stats.recordDisconnect(transportErr)
		}
		ws.Close()
		s.mu.Lock()
		s.active = nil
		s.activeCloseProof = ""
		s.signalLocked()
		s.mu.Unlock()
	}()
	defaultClose := ws.CloseHandler()
	ws.SetCloseHandler(func(code int, text string) error {
		// Unauthenticated Close frames must not destroy resumable TCP state.
		if !crypt.client && code == websocket.CloseNormalClosure && hmac.Equal([]byte(text), []byte(sessionClosePrefix+crypt.closeProof)) {
			s.close()
			return errPeerSessionClosed
		}
		return defaultClose(code, text)
	})
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
	writerStopped := make(chan struct{})
	go func() {
		err := s.writeTransport(ws, crypt, done, pongs)
		ws.Close()
		close(writerStopped)
		writerDone <- err
	}()
	err := s.readTransport(ws, crypt, touch, confirm, writerStopped)
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
	if s.opts.Stats != nil {
		unregister := s.opts.Stats.registerNoiseTransport(s)
		defer unregister()
	}

	ticker := time.NewTicker(s.opts.Heartbeat)
	defer ticker.Stop()
	// 一个写协程仅持有一个噪音定时器；关闭发送时停止，不遗留待发大包。
	noiseTimer := time.NewTimer(time.Hour)
	defer noiseTimer.Stop()
	noiseTimer.Stop()
	var noiseTicks <-chan time.Time
	options, changed := s.opts.liveSnapshot()
	resetNoise := func() error {
		noiseTimer.Stop()
		select {
		case <-noiseTimer.C:
		default:
		}
		noiseTicks = nil
		delay, err := options.noiseDelay()
		if err != nil {
			return err
		}
		if delay > 0 {
			noiseTimer.Reset(delay)
			noiseTicks = noiseTimer.C
		}
		return nil
	}
	if err := resetNoise(); err != nil {
		return err
	}
	noiseDue := false
	noiseRemaining := 0
	refreshNoise := func() error {
		next, nextChanged := s.opts.liveSnapshot()
		timingChanged := options.NoiseInterval != next.NoiseInterval ||
			options.NoiseIntervalMin != next.NoiseIntervalMin ||
			options.NoiseIntervalMax != next.NoiseIntervalMax || !options.sameNoisePolicy(next)
		options, changed = next, nextChanged
		// 大小修改用于下一批噪音；重连参数不影响当前传输的噪音时钟。
		// 只有启停、间隔或场景策略变化才重新计时，避免无关保存饿死噪音发送。
		if !timingChanged {
			return nil
		}
		noiseDue, noiseRemaining = false, 0
		return resetNoise()
	}
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
		select {
		case <-changed:
			if err := refreshNoise(); err != nil {
				return err
			}
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
					if next.op == opData {
						if s.opts.Stats != nil {
							s.opts.Stats.businessTX.Add(uint64(len(next.data)))
							s.opts.Stats.peerPayload(ws.NetConn(), "businessTX", len(next.data))
						}
						s.noteBusiness()
					}
					sent = next.seq
				}
				lastAck = ack
			}
			// 每个小块前重新判定，流量恢复或最后一条业务流关闭时停止禁用场景的余量。
			if noiseDue && !s.noiseAllowed(options, time.Now()) {
				noiseDue, noiseRemaining = false, 0
				if err := resetNoise(); err != nil {
					return err
				}
			}
			if noiseDue {
				if noiseRemaining == 0 {
					noiseRemaining, err = options.noiseSize()
					if err != nil {
						return err
					}
				}
				size := min(noiseRemaining, chunkSize)
				if err := s.writeNoise(ws, crypt, size); err != nil {
					return err
				}
				noiseRemaining -= size
				// 每块之间返回循环，处理业务、ACK、Ping/Pong 和取消。
				if noiseRemaining > 0 {
					continue
				}
				noiseDue = false
				// Start a fresh wait after sending; never accumulate overdue padding.
				delay, err := options.noiseDelay()
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
		case <-changed:
			if err := refreshNoise(); err != nil {
				return err
			}
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

// readTransport 先处理ACK，再将可靠帧按序放入有界队列；噪音就地丢弃，不交给业务TCP。
func (s *session) readTransport(
	ws *websocket.Conn,
	crypt *codec,
	touch, confirm func() error,
	stopped <-chan struct{},
) error {
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
		// 按实际认证解密后的线帧负载计数，重放也计入；管理及控制流量不算业务。
		if f.op == opData {
			if s.opts.Stats != nil {
				s.opts.Stats.businessRX.Add(uint64(len(f.data)))
				s.opts.Stats.peerPayload(ws.NetConn(), "businessRX", len(f.data))
			}
			s.noteBusiness()
		}
		if f.op == opNoise {
			if err := confirm(); err != nil {
				return err
			}
			// Padding belongs only to this transport, never to reliable streams.
			if s.opts.Stats != nil {
				s.opts.Stats.noiseRX.Add(uint64(len(f.data)))
				s.opts.Stats.peerPayload(ws.NetConn(), "noiseRX", len(f.data))
			}
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
		if err := s.queueIncoming(f, stopped); err != nil {
			return err
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
	inFlight := 0
	for _, f := range s.pending {
		cost := frameHeader + len(f.data)
		if f.seq <= sent {
			inFlight += cost
			continue
		}
		if s.peerWindow > 0 && inFlight+cost > s.peerWindow {
			break
		}
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
