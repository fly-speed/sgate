package tunnel

import (
	"container/heap"
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type impairmentProfile struct {
	Delay          time.Duration `json:"one_way_delay_ns"`
	DropEvery      int           `json:"drop_every"`
	ReorderEvery   int           `json:"reorder_every"`
	ReorderDelay   time.Duration `json:"reorder_delay_ns"`
	BytesPerSecond int           `json:"bytes_per_second"`
	MaxDatagram    int           `json:"max_udp_payload"`
}

// Real datagrams, bounded queues, deterministic impairments and NAT rebinding.
// This is not an emulation of IP loss on a TCP connection.
type udpImpairment struct {
	listener                                          *net.UDPConn
	mu                                                sync.Mutex
	upstream                                          *net.UDPConn
	target                                            *net.UDPAddr
	ctx                                               context.Context
	client                                            atomic.Pointer[net.UDPAddr]
	drops, mtuDrops, reordered, forwarded, rebindings atomic.Uint64
	blackhole                                         atomic.Bool
	cancel                                            context.CancelFunc
	wg                                                sync.WaitGroup
	profile                                           impairmentProfile
}
type delayedDatagram struct {
	data  []byte
	due   time.Time
	index uint64
}
type datagramHeap []delayedDatagram

func (h datagramHeap) Len() int           { return len(h) }
func (h datagramHeap) Less(i, j int) bool { return h[i].due.Before(h[j].due) }
func (h datagramHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *datagramHeap) Push(v any)        { *h = append(*h, v.(delayedDatagram)) }
func (h *datagramHeap) Pop() any {
	old := *h
	n := len(old)
	p := old[n-1]
	old[n-1] = delayedDatagram{}
	*h = old[:n-1]
	return p
}
func newUDPImpairment(t *testing.T, target string, delay time.Duration, dropEvery int) *udpImpairment {
	return newUDPProfile(t, target, impairmentProfile{Delay: delay, DropEvery: dropEvery})
}
func newUDPProfile(t *testing.T, target string, profile impairmentProfile) *udpImpairment {
	t.Helper()
	address, err := net.ResolveUDPAddr("udp", target)
	if err != nil {
		t.Fatal(err)
	}
	downstream, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &udpImpairment{listener: downstream, target: address, ctx: ctx, cancel: cancel, profile: profile}
	if err := r.rebind(); err != nil {
		downstream.Close()
		cancel()
		t.Fatal(err)
	}
	r.rebindings.Store(0)
	r.launch(func(p []byte) (int, error) {
		n, addr, err := downstream.ReadFromUDP(p)
		if addr != nil {
			r.client.Store(addr)
		}
		return n, err
	}, func(p []byte) error { r.mu.Lock(); defer r.mu.Unlock(); _, err := r.upstream.Write(p); return err })
	t.Cleanup(func() {
		cancel()
		r.mu.Lock()
		r.upstream.Close()
		r.mu.Unlock()
		downstream.Close()
		r.wg.Wait()
	})
	return r
}
func (r *udpImpairment) rebind() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ctx.Err() != nil {
		return r.ctx.Err()
	}
	next, err := net.DialUDP("udp", nil, r.target)
	if err != nil {
		return err
	}
	old := r.upstream
	r.upstream = next
	r.launch(next.Read, func(p []byte) error {
		addr := r.client.Load()
		if addr == nil {
			return nil
		}
		_, err := r.listener.WriteToUDP(p, addr)
		return err
	})
	if old != nil {
		old.Close()
	}
	r.rebindings.Add(1)
	return nil
}
func (r *udpImpairment) launch(read func([]byte) (int, error), write func([]byte) error) {
	queue := make(chan delayedDatagram, 256)
	r.wg.Add(2)
	go func() {
		defer r.wg.Done()
		defer close(queue)
		p := make([]byte, 65536)
		var index uint64
		for {
			n, err := read(p)
			if err != nil {
				return
			}
			index++
			if r.blackhole.Load() || (r.profile.DropEvery > 0 && index%uint64(r.profile.DropEvery) == 0) {
				r.drops.Add(1)
				continue
			}
			if r.profile.MaxDatagram > 0 && n > r.profile.MaxDatagram {
				r.drops.Add(1)
				r.mtuDrops.Add(1)
				continue
			}
			due := time.Now().Add(r.profile.Delay)
			if r.profile.ReorderEvery > 0 && index%uint64(r.profile.ReorderEvery) == 0 {
				due = due.Add(r.profile.ReorderDelay)
			}
			packet := delayedDatagram{append([]byte(nil), p[:n]...), due, index}
			select {
			case queue <- packet:
			case <-r.ctx.Done():
				return
			default:
				r.drops.Add(1)
			}
		}
	}()
	go func() {
		defer r.wg.Done()
		timer := time.NewTimer(time.Hour)
		defer timer.Stop()
		timer.Stop()
		var pending datagramHeap
		var nextSend time.Time
		var last uint64
		for {
			var tick <-chan time.Time
			if len(pending) > 0 {
				due := pending[0].due
				if nextSend.After(due) {
					due = nextSend
				}
				timer.Reset(max(time.Until(due), 0))
				tick = timer.C
			}
			select {
			case <-r.ctx.Done():
				return
			case packet, ok := <-queue:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				if !ok {
					queue = nil
					if len(pending) == 0 {
						return
					}
					continue
				}
				if len(pending) >= 256 {
					r.drops.Add(1)
				} else {
					heap.Push(&pending, packet)
				}
			case <-tick:
				packet := heap.Pop(&pending).(delayedDatagram)
				if err := write(packet.data); err != nil {
					if r.ctx.Err() != nil {
						return
					}
					r.drops.Add(1)
					continue
				}
				if packet.index < last {
					r.reordered.Add(1)
				}
				last = max(last, packet.index)
				r.forwarded.Add(1)
				if r.profile.BytesPerSecond > 0 {
					nextSend = time.Now().Add(time.Duration(float64(len(packet.data)) / float64(r.profile.BytesPerSecond) * float64(time.Second)))
				}
				if queue == nil && len(pending) == 0 {
					return
				}
			}
		}
	}()
}
