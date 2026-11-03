package tunnel

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
)

type nativeControl struct {
	Kind string `json:"kind"`
	ID   uint64 `json:"id,omitempty"`
	Seq  uint64 `json:"seq,omitempty"`
	Op   byte   `json:"op,omitempty"`
	Data []byte `json:"data,omitempty"`
}
type nativeTransport struct {
	pongs                  chan struct{}
	replies                chan nativeControl
	traffic                *nativeTraffic
	conn                   *quic.Conn
	control                *quic.Stream
	crypt                  *codec
	key                    []byte
	id, nonce, serverNonce string
	peerLimit              int
	writeMu                sync.Mutex
	ctx                    context.Context
	cancel                 context.CancelFunc
	workers                sync.WaitGroup
	channelsMu             sync.Mutex
	channels               map[uint64]bool
	errors                 chan error
	stopOnce               sync.Once
	s                      *session
}

func newNativeTransport(conn *quic.Conn, control *quic.Stream, crypt *codec, key []byte, id, nonce, serverNonce string) *nativeTransport {
	ctx, cancel := context.WithCancel(conn.Context())
	return &nativeTransport{conn: conn, control: control, crypt: crypt, key: key, id: id, nonce: nonce, serverNonce: serverNonce, ctx: ctx, cancel: cancel, channels: make(map[uint64]bool), pongs: make(chan struct{}, 1), errors: make(chan error, 1)}
}
func (nt *nativeTransport) fail(err error) {
	select {
	case nt.errors <- err:
	default:
	}
	nt.stop()
}

const nativeSessionCloseCode quic.ApplicationErrorCode = 0x5347

func (nt *nativeTransport) stop() {
	nt.stopOnce.Do(func() {
		nt.cancel()
		nt.control.CancelRead(0)
		_ = nt.control.SetWriteDeadline(time.Now())
		nt.conn.CloseWithError(0, "transport ended")
	})
}

// Application close is QUIC-authenticated and bypasses a congested control
// stream. The additional proof binds intentional teardown to this session and
// transport. Ordinary network failures never destroy resumable business state.
func (nt *nativeTransport) endSession() {
	if nt.crypt.client && nt.s.opts.closeSessionOnStop {
		nt.conn.CloseWithError(nativeSessionCloseCode, nt.crypt.closeProof)
	}
	nt.stop()
}
func (nt *nativeTransport) peerEnded(err error) bool {
	if nt.crypt.client {
		return false
	}
	var closed *quic.ApplicationError
	for _, candidate := range []error{err, context.Cause(nt.conn.Context())} {
		if errors.As(candidate, &closed) && closed.Remote && closed.ErrorCode == nativeSessionCloseCode && hmac.Equal([]byte(closed.ErrorMessage), []byte(nt.crypt.closeProof)) {
			return true
		}
	}
	return false
}
func (nt *nativeTransport) send(v nativeControl) error {
	p, err := json.Marshal(v)
	if err != nil {
		return err
	}
	nt.writeMu.Lock()
	defer nt.writeMu.Unlock()
	if nt.ctx.Err() != nil {
		return nt.ctx.Err()
	}
	_ = nt.control.SetWriteDeadline(time.Now().Add(nt.s.opts.WriteTimeout))
	sealed := nt.crypt.seal(p)
	err = writeNativeRecord(nativeIO{ReadWriter: nt.control, traffic: nt.traffic}, sealed)

	return err
}
func (s *session) runNative(nt *nativeTransport, resumeDeadline time.Time, onReady func()) (result error) {
	defer nt.traffic.close()
	nt.replies = make(chan nativeControl, min(4096, max(128, s.opts.MaxStreams)))
	s.transportMu.Lock()
	defer s.transportMu.Unlock()
	s.mu.Lock()
	if s.closed || s.native == nil {
		s.mu.Unlock()
		nt.stop()
		return errSessionClosed
	}
	s.native.active = nt
	nt.s = s
	s.reportStreamErrors = true
	s.signalLocked()
	s.mu.Unlock()
	defer func() {
		nt.stop()
		nt.workers.Wait()
		s.mu.Lock()
		if s.native.active == nt {
			s.native.active = nil
		}
		s.signalLocked()
		s.mu.Unlock()
		if s.opts.Stats != nil {
			s.opts.Stats.recordDisconnect(result)
		}
	}()
	var confirmation atomic.Uint32
	var timer *time.Timer
	if !resumeDeadline.IsZero() {
		timer = time.AfterFunc(time.Until(resumeDeadline), func() {
			if confirmation.CompareAndSwap(0, 2) {
				nt.fail(errors.New("native resume confirmation expired"))
			}
		})
		defer timer.Stop()
	}
	nt.workers.Add(2)
	go func() {
		defer nt.workers.Done()
		if err := nt.writeControls(); err != nil {
			nt.fail(err)
		}
	}()
	go func() {
		defer nt.workers.Done()
		if err := nt.acceptChannels(); err != nil {
			nt.fail(err)
		}
	}()
	ready := false
	for {
		_ = nt.control.SetReadDeadline(time.Now().Add(s.opts.HeartbeatTimeout))
		p, err := readNativeRecord(progressReader{Reader: nativeIO{ReadWriter: nt.control, traffic: nt.traffic}, touch: func() error { return nt.control.SetReadDeadline(time.Now().Add(s.opts.HeartbeatTimeout)) }}, frameHeader+chunkSize+8192)
		if err != nil {
			if nt.peerEnded(err) {
				s.close()
				return errPeerSessionClosed
			}
			select {
			case cause := <-nt.errors:
				return cause
			default:
				return err
			}
		}
		p, err = nt.crypt.open(p)
		if err != nil {
			return fmt.Errorf("native control decrypt: %w", err)
		}
		var control nativeControl
		if json.Unmarshal(p, &control) != nil {
			return errors.New("invalid native control")
		}
		if !ready {
			if control.Kind != "ready" {
				return errors.New("native channel not confirmed")
			}
			if !resumeDeadline.IsZero() && !time.Now().Before(resumeDeadline) {
				return errors.New("native resume confirmation expired")
			}
			if !confirmation.CompareAndSwap(0, 1) {
				return errors.New("native resume confirmation expired")
			}
			if timer != nil {
				timer.Stop()
			}
			ready = true
			if onReady != nil {
				onReady()
			}
			continue
		}
		switch control.Kind {
		case "open":
			if err := nt.receiveOpen(control); err != nil {
				return err
			}
		case "ack":
			if err := s.nativeAcknowledge(control.ID, control.Seq); err != nil {
				return err
			}
		case "retired":
			s.mu.Lock()
			flow := s.native.flows[control.ID]
			valid := flow != nil && flow.local
			s.mu.Unlock()
			if !valid {
				continue
			}
			s.drop(control.ID)
			s.mu.Lock()
			s.nativeForgetLocked(control.ID)
			s.mu.Unlock()
		case "ping":
			select {
			case nt.pongs <- struct{}{}:
			default:
			}
		case "pong":
		case "noise":
			if len(control.Data) == 0 || len(control.Data) > chunkSize {
				return errors.New("invalid native noise")
			}
			if s.opts.Stats != nil {
				s.opts.Stats.noiseRX.Add(uint64(len(control.Data)))
				if nt.traffic.peer != nil {
					nt.traffic.peer.noiseRX.Add(uint64(len(control.Data)))
				}
			}
			s.mu.Lock()
			s.noiseReceived++
			s.mu.Unlock()
		case "close":
			if nt.crypt.client || !hmac.Equal(control.Data, []byte(nt.crypt.closeProof)) {
				return errors.New("invalid native session close")
			}
			s.close()
			return errPeerSessionClosed
		default:
			return errors.New("unsupported native control")
		}
	}
}
func (nt *nativeTransport) receiveOpen(c nativeControl) error {
	s := nt.s
	if c.Seq != 1 || c.ID == 0 || c.ID == reverseStreamBase || len(c.Data) > 1024 || (c.Op != opOpen && c.Op != opReverseOpen && c.Op != opReverseOpenTLS) {
		return errors.New("invalid native OPEN")
	}
	if (nt.crypt.client && c.ID < reverseStreamBase) || (!nt.crypt.client && c.ID >= reverseStreamBase) {
		return errors.New("invalid native stream direction")
	}
	if c.Op == opOpen && len(c.Data) != 0 {
		return errors.New("native forward OPEN has target")
	}
	if c.Op != opOpen && (!s.opts.receiveReverse || c.ID < reverseStreamBase || validateSelectableTarget(string(c.Data)) != nil) {
		return errors.New("native reverse OPEN not permitted")
	}
	if c.Op == opOpen && nt.crypt.client {
		return errors.New("native forward OPEN from server")
	}
	s.mu.Lock()
	if flow := s.native.flows[c.ID]; flow != nil {
		same := !flow.local && flow.open.op == c.Op && string(flow.open.data) == string(c.Data)
		s.mu.Unlock()
		if !same {
			return errors.New("native OPEN changed on resume")
		}
		return nil
	}
	if c.ID <= s.lastOpened {
		s.mu.Unlock()
		select {
		case nt.replies <- nativeControl{Kind: "retired", ID: c.ID}:
			return nil
		default:
			return errors.New("native control reply limit reached")
		}
	}
	// Both retained metadata and live backend sockets are bounded.
	if len(s.native.flows) >= s.opts.MaxStreams {
		s.lastOpened = c.ID
		s.mu.Unlock()
		select {
		case nt.replies <- nativeControl{Kind: "retired", ID: c.ID, Data: []byte{resetStreamLimit}}:
			return nil
		default:
			return errors.New("native control reply limit reached")
		}
	}
	f := &nativeFlow{id: c.ID, open: frame{op: c.Op, stream: c.ID, seq: 1, data: append([]byte(nil), c.Data...)}}
	cost := frameHeader + len(f.open.data)
	if s.incomingBytes+cost > s.opts.ReceiveBytes {
		s.lastOpened = c.ID
		s.mu.Unlock()
		select {
		case nt.replies <- nativeControl{Kind: "retired", ID: c.ID, Data: []byte{resetStreamLimit}}:
			return nil
		default:
			return errors.New("native control reply limit reached")
		}
	}
	f.incoming = []frame{f.open}
	f.incomingBytes = cost
	f.accepted = 1
	s.incomingBytes += cost
	s.lastOpened = c.ID
	s.native.flows[c.ID] = f
	s.signalLocked()
	s.mu.Unlock()
	go s.nativeDeliver(f)
	return nil
}
func (nt *nativeTransport) writeControls() error {
	s := nt.s
	if err := nt.send(nativeControl{Kind: "ready"}); err != nil {
		return err
	}
	opens := make(map[uint64]bool)
	acks := make(map[uint64]uint64)
	heartbeat := time.NewTicker(s.opts.Heartbeat)
	defer heartbeat.Stop()
	noise := time.NewTimer(time.Hour)
	defer noise.Stop()
	noise.Stop()
	options, changed := s.opts.liveSnapshot()
	var noiseTicks <-chan time.Time
	pacer := quicNoisePacer{}
	refreshNoise := func() error { options, changed = s.opts.liveSnapshot(); return nil }
	if s.opts.Stats != nil {
		defer s.opts.Stats.registerNoiseTransport(s)()
	}
	for {
		select {
		case <-changed:
			if err := refreshNoise(); err != nil {
				return err
			}
		default:
		}
		select {
		case <-nt.pongs:
			if err := nt.send(nativeControl{Kind: "pong"}); err != nil {
				return err
			}
		default:
		}
		select {
		case reply := <-nt.replies:
			if err := nt.send(reply); err != nil {
				return err
			}
		default:
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return errSessionClosed
		}
		ids := make([]uint64, 0, len(s.native.flows))
		for id := range s.native.flows {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		controls := make([]nativeControl, 0, len(ids)*2)
		var start []*nativeFlow
		for _, id := range ids {
			f := s.native.flows[id]
			if f.local && !opens[id] {
				controls = append(controls, nativeControl{Kind: "open", ID: id, Seq: 1, Op: f.open.op, Data: f.open.data})
				opens[id] = true
			}
			if f.received > acks[id] {
				controls = append(controls, nativeControl{Kind: "ack", ID: id, Seq: f.received})
				acks[id] = f.received
			}
			if !f.resetPending && s.streams[id] == nil && len(f.incoming) == 0 && len(f.pending) == 0 && f.received == f.accepted {
				s.nativeForgetLocked(id)
				delete(acks, id)
			} else if f.local && f.acked >= 1 {
				start = append(start, f)
			}
		}
		wake := s.changed
		s.mu.Unlock()
		for _, control := range controls {
			if err := nt.send(control); err != nil {
				return err
			}
		}
		for _, flow := range start {
			nt.startChannel(flow, nil)
		}
		// Drop transport-local book-keeping after persistent flow retirement.
		for id := range opens {
			s.mu.Lock()
			_, exists := s.native.flows[id]
			s.mu.Unlock()
			if !exists {
				delete(opens, id)
				delete(acks, id)
			}
		}
		size, wait, err := pacer.poll(time.Now(), options, s.noiseAllowed(options, time.Now()))
		if err != nil {
			return err
		}
		if size > 0 {
			data := make([]byte, size)
			if _, err := rand.Read(data); err != nil {
				return err
			}
			if err := nt.send(nativeControl{Kind: "noise", Data: data}); err != nil {
				return err
			}
			pacer.sent(time.Now())
			if s.opts.Stats != nil {
				s.opts.Stats.noiseTX.Add(uint64(size))
				if nt.traffic.peer != nil {
					nt.traffic.peer.noiseTX.Add(uint64(size))
				}
			}
			s.mu.Lock()
			s.noiseSent++
			s.mu.Unlock()
			// Always return through control/heartbeat handling before the next frame.
			select {
			case <-nt.ctx.Done():
				return nt.ctx.Err()
			case <-heartbeat.C:
				if err := nt.send(nativeControl{Kind: "ping"}); err != nil {
					return err
				}
			default:
			}
			continue
		}
		noise.Stop()
		select {
		case <-noise.C:
		default:
		}
		noiseTicks = nil
		if wait > 0 {
			noise.Reset(wait)
			noiseTicks = noise.C
		}
		select {
		case <-nt.pongs:
			if err := nt.send(nativeControl{Kind: "pong"}); err != nil {
				return err
			}
		case reply := <-nt.replies:
			if err := nt.send(reply); err != nil {
				return err
			}
		case <-nt.ctx.Done():
			return nt.ctx.Err()
		case <-s.ctx.Done():
			return s.ctx.Err()
		case <-wake:
		case <-heartbeat.C:
			if err := nt.send(nativeControl{Kind: "ping"}); err != nil {
				return err
			}
		case <-changed:
			if err := refreshNoise(); err != nil {
				return err
			}
		case <-noiseTicks:
			// Re-evaluate pacing, policy and live settings on the next iteration.
		}
	}
}
func (nt *nativeTransport) acceptChannels() error {
	for {
		channel, err := nt.conn.AcceptStream(nt.ctx)
		if err != nil {
			return err
		}
		nt.workers.Add(1)
		go func() {
			defer nt.workers.Done()
			_ = channel.SetReadDeadline(time.Now().Add(nt.s.opts.DialTimeout))
			var h [8]byte
			if _, err := io.ReadFull(nativeIO{ReadWriter: channel, traffic: nt.traffic}, h[:]); err != nil {
				channel.CancelRead(1)
				channel.CancelWrite(1)
				return
			}
			id := binary.BigEndian.Uint64(h[:])
			nt.s.mu.Lock()
			flow := nt.s.native.flows[id]
			valid := flow != nil && !flow.local
			nt.s.mu.Unlock()
			if !valid {
				channel.CancelRead(1)
				channel.CancelWrite(1)
				return
			}
			_ = channel.SetReadDeadline(time.Time{})
			nt.startChannel(flow, channel)
		}()
	}
}
func (nt *nativeTransport) startChannel(flow *nativeFlow, channel *quic.Stream) {
	nt.s.mu.Lock()
	valid := nt.s.native.flows[flow.id] == flow
	nt.s.mu.Unlock()
	if !valid {
		if channel != nil {
			channel.CancelRead(0)
			channel.CancelWrite(0)
		}
		return
	}
	nt.channelsMu.Lock()
	if nt.channels[flow.id] {
		nt.channelsMu.Unlock()
		if channel != nil {
			channel.CancelRead(1)
			channel.CancelWrite(1)
		}
		return
	}
	nt.channels[flow.id] = true
	nt.channelsMu.Unlock()
	nt.workers.Add(1)
	go func() {
		defer nt.workers.Done()
		if channel == nil {
			var err error
			channel, err = nt.conn.OpenStreamSync(nt.ctx)
			if err != nil {
				nt.fail(err)
				return
			}
			_ = channel.SetWriteDeadline(time.Now().Add(nt.s.opts.WriteTimeout))
			var h [8]byte
			binary.BigEndian.PutUint64(h[:], flow.id)
			if _, err := (nativeIO{ReadWriter: channel, traffic: nt.traffic}).Write(h[:]); err != nil {
				nt.fail(err)
				return
			}
		}
		if stats := nt.s.opts.Stats; stats != nil {
			stats.quicStreams.Add(1)
		}
		crypt := newCodec(nt.key, nt.id+"/native/"+strconv.FormatUint(flow.id, 10), nt.nonce, nt.serverNonce, nt.crypt.client)
		writeDone := make(chan error, 1)
		go func() {
			err := nt.writeChannel(flow, channel, crypt)
			nt.s.mu.Lock()
			retired := nt.s.native.flows[flow.id] != flow
			nt.s.mu.Unlock()
			if err != nil && !retired && nt.ctx.Err() == nil {
				nt.fail(err)
			}
			writeDone <- err
		}()
		err := nt.readChannel(flow, channel, crypt)
		nt.s.mu.Lock()
		retired := nt.s.native.flows[flow.id] != flow
		remoteEnded := flow.remoteEnded || retired
		nt.s.mu.Unlock()
		if err != nil && !retired && (!errors.Is(err, io.EOF) || !remoteEnded) && nt.ctx.Err() == nil {
			nt.fail(err)
		}
		writerErr := <-writeDone
		channel.CancelRead(0)
		channel.CancelWrite(0)
		nt.channelsMu.Lock()
		delete(nt.channels, flow.id)
		nt.channelsMu.Unlock()
		if nt.ctx.Err() == nil {
			// Normal flow retirement stops its channel; other unexpected stream errors
			// reconnect the transport, retaining all per-flow replay state.
			nt.s.mu.Lock()
			retired := nt.s.native.flows[flow.id] != flow
			nt.s.mu.Unlock()
			if !retired {
				nt.fail(errors.Join(err, writerErr))
			}
		}
	}()
}
func (nt *nativeTransport) writeChannel(flow *nativeFlow, channel *quic.Stream, crypt *codec) error {
	s := nt.s
	var sent uint64
	if flow.local {
		sent = 1
	}
	for {
		s.mu.Lock()
		if s.closed || s.native.flows[flow.id] != flow {
			s.mu.Unlock()
			_ = channel.Close()
			return nil
		}
		var next frame
		found := false
		inFlight := 0
		for _, f := range flow.pending {
			if f.seq <= sent {
				inFlight += frameHeader + len(f.data)
				continue
			}
			if inFlight+frameHeader+len(f.data) > nt.peerLimit {
				break
			}
			next = f
			found = true
			break
		}
		changed := s.changed
		replay := found && next.seq <= flow.sentHigh
		s.mu.Unlock()
		if !found {
			select {
			case <-nt.ctx.Done():
				return nt.ctx.Err()
			case <-changed:
				continue
			}
		}
		_ = channel.SetWriteDeadline(time.Now().Add(s.opts.WriteTimeout))
		sealed := crypt.seal(next.encode())
		if err := writeNativeRecord(nativeIO{ReadWriter: channel, traffic: nt.traffic}, sealed); err != nil {
			return err
		}
		sent = next.seq
		s.mu.Lock()
		flow.sentHigh = max(flow.sentHigh, sent)
		if next.op == opData {
			s.lastBusiness = time.Now()
		}
		s.mu.Unlock()
		if stats := s.opts.Stats; stats != nil {
			if next.op == opData {
				stats.businessTX.Add(uint64(len(next.data)))
				if nt.traffic.peer != nil {
					nt.traffic.peer.businessTX.Add(uint64(len(next.data)))
				}
				if replay {
					stats.quicReplay.Add(uint64(len(next.data)))
				}
			}
		}
	}
}
func (nt *nativeTransport) readChannel(flow *nativeFlow, channel *quic.Stream, crypt *codec) error {
	s := nt.s
	for {
		p, err := readNativeRecord(nativeIO{ReadWriter: channel, traffic: nt.traffic}, frameHeader+chunkSize+24)
		if err != nil {
			return err
		}
		p, err = crypt.open(p)
		if err != nil {
			return err
		}
		f, err := decodeFrame(p)
		if err != nil {
			return err
		}
		if f.stream != flow.id || f.ack != 0 || (f.op != opData && f.op != opFin && f.op != opReset) {
			return errors.New("invalid native business frame")
		}
		if f.op == opData {
			if stats := s.opts.Stats; stats != nil {
				stats.businessRX.Add(uint64(len(f.data)))
				if nt.traffic.peer != nil {
					nt.traffic.peer.businessRX.Add(uint64(len(f.data)))
				}
			}
			s.noteBusiness()
		}
		if err := s.nativeIncoming(f, nt.ctx.Done()); err != nil {
			return err
		}
	}
}
