package tunnel

import (
	"errors"
	"sync"
)

var ErrMemoryBudget = errors.New("tunnel managed-memory budget exhausted")

// MemoryBudget is shared by all mappings, retained sessions and QUIC connections
// in a process. It accounts declared buffer capacities, not Go/OS RSS.
type MemoryBudget struct {
	mu                          sync.Mutex
	limit, used, peak, rejected uint64
}
type MemorySnapshot struct {
	Limit    uint64 `json:"limit_bytes"`
	Reserved uint64 `json:"reserved_bytes"`
	Peak     uint64 `json:"peak_reserved_bytes"`
	Rejected uint64 `json:"rejected_reservations"`
}

func NewMemoryBudget(limit uint64) *MemoryBudget { return &MemoryBudget{limit: limit} }
func (b *MemoryBudget) Snapshot() MemorySnapshot {
	if b == nil {
		return MemorySnapshot{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return MemorySnapshot{b.limit, b.used, b.peak, b.rejected}
}
func (b *MemoryBudget) Configure(limit uint64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used > 0 && limit != b.limit {
		return errors.New("memory-budget-bytes change requires a process restart")
	}
	b.limit = limit
	return nil
}
func (b *MemoryBudget) reserve(n uint64) (func(), error) {
	if b == nil {
		return func() {}, nil
	}
	b.mu.Lock()
	if n > ^uint64(0)-b.used || (b.limit > 0 && (n > b.limit || b.used > b.limit-n)) {
		b.rejected++
		b.mu.Unlock()
		return nil, ErrMemoryBudget
	}
	b.used += n
	b.peak = max(b.peak, b.used)
	b.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { b.mu.Lock(); b.used -= n; b.mu.Unlock() }) }, nil
}
func (o Options) resourceDefaults() (Options, error) {
	for _, n := range []int{o.BufferBytes, o.QUICStreamWindow, o.QUICConnectionWindow, o.NativeStreamBuffer, o.ReplayBytes, o.ReceiveBytes, o.MemoryBudgetBytes} {
		if n < 0 || uint64(n) > 1<<40 {
			return o, errors.New("resource sizes must be in 0..1TiB")
		}
	}
	if o.QUICInitialPacketSize == 0 {
		o.QUICInitialPacketSize = 1200
	}
	if o.QUICInitialPacketSize < 1200 || o.QUICInitialPacketSize > 1452 {
		return o, errors.New("quic-initial-packet-bytes must be in 1200..1452")
	}
	if o.QUICStreamWindow == 0 {
		o.QUICStreamWindow = o.BufferBytes
	}
	if o.QUICConnectionWindow == 0 {
		o.QUICConnectionWindow = o.BufferBytes
	}
	if o.NativeStreamBuffer == 0 {
		o.NativeStreamBuffer = max(frameHeader+chunkSize, o.BufferBytes/8)
	}
	if o.ReplayBytes == 0 {
		o.ReplayBytes = o.BufferBytes
	}
	if o.ReceiveBytes == 0 {
		o.ReceiveBytes = o.BufferBytes
	}
	if o.QUICStreamWindow < 16384 || o.QUICConnectionWindow < 16384 || o.NativeStreamBuffer < frameHeader+chunkSize || o.ReplayBytes < o.NativeStreamBuffer || o.ReceiveBytes < o.NativeStreamBuffer {
		return o, errors.New("invalid QUIC windows or queue budgets: stream buffer must fit replay and receive budgets")
	}
	if o.MaxStreams > 1<<20 {
		return o, errors.New("stream limit exceeds 1048576")
	}
	if o.Budget == nil {
		if o.Stats != nil {
			o.Budget = o.Stats.MemoryBudget()
			if o.MemoryBudgetBytes > 0 && o.Budget.Snapshot().Limit != uint64(o.MemoryBudgetBytes) {
				if err := o.Budget.Configure(uint64(o.MemoryBudgetBytes)); err != nil {
					return o, err
				}
			}
		} else {
			o.Budget = NewMemoryBudget(uint64(o.MemoryBudgetBytes))
		}
	}
	return o, nil
}
func (o Options) sessionReservation() uint64 {
	queues := uint64(o.BufferBytes) * 2
	if o.nativeQUIC {
		queues = uint64(o.ReplayBytes) + uint64(o.ReceiveBytes)
	}
	// Conservative capacity for per-stream TCP reads and transport frame decoding.
	return queues + uint64(o.MaxStreams)*uint64(2*(chunkSize+frameHeader+24))
}
func (s *Stats) MemoryBudget() *MemoryBudget {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.budget == nil {
		s.budget = NewMemoryBudget(0)
	}
	return s.budget
}

func (s *session) replayLimit() int {
	if s.native != nil {
		return s.opts.ReplayBytes
	}
	return s.opts.BufferBytes
}
