package tunnel

import (
	"errors"
	"time"
)

// All persistent flow state is protected by session.mu and survives transports.
// Independent sequence/ACK spaces prevent a stalled backend from ordering the
// delivery or replay of unrelated business connections.
type nativeState struct {
	flows  map[uint64]*nativeFlow
	active *nativeTransport
}
type nativeFlow struct {
	remoteEnded                               bool
	resetPending                              bool
	id                                        uint64
	local                                     bool
	open                                      frame
	next, received, accepted, acked, sentHigh uint64
	pending, incoming                         []frame
	pendingBytes, incomingBytes               int
}

func (s *session) nativeLimit() int { return s.opts.NativeStreamBuffer }
func (s *session) nativeEnqueueLocked(op byte, id uint64, data []byte) {
	f := s.native.flows[id]
	if f == nil {
		f = &nativeFlow{id: id, local: true}
		s.native.flows[id] = f
		go s.nativeDeliver(f)
	}
	if op == opReset {
		f.resetPending = false
	}
	if op == opData {
		s.lastBusiness = time.Now()
	}
	f.next++
	next := frame{op: op, stream: id, seq: f.next, data: append([]byte(nil), data...)}
	if op == opOpen || op == opReverseOpen || op == opReverseOpenTLS {
		f.open = next
	}
	f.pending = append(f.pending, next)
	cost := frameHeader + len(data)
	f.pendingBytes += cost
	s.pendingBytes += cost
	s.signalLocked()
}
func (s *session) nativeEnqueue(op byte, id uint64, data []byte, st *stream) error {
	cost := frameHeader + len(data)
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
		f := s.native.flows[id]
		if f == nil {
			s.mu.Unlock()
			if op == opReset {
				return nil
			}
			return errStreamClosed
		}
		if s.pendingBytes+cost <= s.opts.ReplayBytes && f.pendingBytes+cost <= s.nativeLimit() {
			s.nativeEnqueueLocked(op, id, data)
			s.mu.Unlock()
			return nil
		}
		changed := s.changed
		s.mu.Unlock()
		if err := s.waitNativeBudget(changed, nil); err != nil {
			return err
		}
	}
}
func (s *session) nativeIncoming(f frame, stopped <-chan struct{}) error {
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return errSessionClosed
		}
		flow := s.native.flows[f.stream]
		if flow == nil {
			s.mu.Unlock()
			return errStreamClosed
		}
		if f.seq <= flow.accepted {
			s.signalLocked()
			s.mu.Unlock()
			return nil
		}
		if f.seq != flow.accepted+1 {
			s.mu.Unlock()
			return errReceiveGap
		}
		cost := frameHeader + len(f.data)
		if s.incomingBytes+cost <= s.opts.ReceiveBytes && flow.incomingBytes+cost <= s.nativeLimit() {
			flow.incoming = append(flow.incoming, f)
			flow.incomingBytes += cost
			s.incomingBytes += cost
			flow.accepted = f.seq
			if f.op == opFin || f.op == opReset {
				flow.remoteEnded = true
			}
			s.signalLocked()
			s.mu.Unlock()
			return nil
		}
		changed := s.changed
		s.mu.Unlock()
		if err := s.waitNativeBudget(changed, stopped); err != nil {
			return err
		}
	}
}
func (s *session) nativeDeliver(flow *nativeFlow) {
	for {
		s.mu.Lock()
		if s.closed || s.native.flows[flow.id] != flow {
			s.mu.Unlock()
			return
		}
		if len(flow.incoming) == 0 {
			changed := s.changed
			s.mu.Unlock()
			select {
			case <-s.ctx.Done():
				return
			case <-changed:
				continue
			}
		}
		f := flow.incoming[0]
		s.mu.Unlock()
		if err := s.apply(f); err != nil {
			s.opts.Logf("native stream=%d delivery failed: %v", flow.id, err)
			s.reset(flow.id)
		}
		s.mu.Lock()
		if s.closed || s.native.flows[flow.id] != flow {
			s.mu.Unlock()
			return
		}
		cost := frameHeader + len(f.data)
		flow.incoming[0] = frame{}
		flow.incoming = flow.incoming[1:]
		if len(flow.incoming) == 0 {
			flow.incoming = nil
		}
		flow.incomingBytes -= cost
		s.incomingBytes -= cost
		flow.received = f.seq
		s.signalLocked()
		s.mu.Unlock()
	}
}
func (s *session) nativeAcknowledge(id, ack uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.native.flows[id]
	if f == nil {
		return nil
	}
	if ack > f.next {
		return errors.New("native ACK beyond sent sequence")
	}
	if ack <= f.acked {
		return nil
	}
	f.acked = ack
	n := 0
	for n < len(f.pending) && f.pending[n].seq <= ack {
		cost := frameHeader + len(f.pending[n].data)
		f.pendingBytes -= cost
		s.pendingBytes -= cost
		f.pending[n] = frame{}
		n++
	}
	f.pending = f.pending[n:]
	if len(f.pending) == 0 {
		f.pending = nil
	}
	s.signalLocked()
	return nil
}
func (s *session) nativeForgetLocked(id uint64) {
	if f := s.native.flows[id]; f != nil {
		s.pendingBytes -= f.pendingBytes
		s.incomingBytes -= f.incomingBytes
		delete(s.native.flows, id)
		s.signalLocked()
	}
}
