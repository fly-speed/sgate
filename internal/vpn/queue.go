package vpn

import (
	"encoding/binary"
	"sync"
	"sync/atomic"
	"time"
)

// Keep IP packets in the fair scheduler until QUIC is ready to send them.
// Optional age expiry is disabled by default; short fixed deadlines can
// repeatedly discard TCP retransmissions on congested high-RTT paths.
const flowQuantum = 1500

type flowKey [13]byte
type queuedPacket struct {
	data []byte
	at   time.Time
}
type packetFlow struct {
	key     flowKey
	packets []queuedPacket
	deficit int
}
type packetQueue struct {
	mu     sync.Mutex
	flows  []*packetFlow
	count  int
	ready  chan struct{}
	maxAge time.Duration
}

func newPacketQueue(maxAge time.Duration) *packetQueue {
	return &packetQueue{ready: make(chan struct{}, 1), maxAge: maxAge}
}
func packetFlowKey(p []byte) (key flowKey) {
	if len(p) < 20 {
		return
	}
	copy(key[:8], p[12:20])
	key[8] = p[9]
	ihl := int(p[0]&15) * 4
	// All fragments of an IPv4 datagram use the same address/protocol bucket.
	if ihl < 20 || ihl > len(p) || binary.BigEndian.Uint16(p[6:8])&0x3fff != 0 {
		return
	}
	switch p[9] {
	case 6, 17:
		if len(p) >= ihl+4 {
			copy(key[9:], p[ihl:ihl+4])
		}
	case 1:
		if len(p) >= ihl+8 {
			key[9] = p[ihl]
			key[10] = p[ihl+1]
			copy(key[11:], p[ihl+4:ihl+6])
		}
	}
	return
}
func storeMax(v *atomic.Uint64, n uint64) {
	for old := v.Load(); n > old; old = v.Load() {
		if v.CompareAndSwap(old, n) {
			return
		}
	}
}
func (q *packetQueue) dropHead(f *packetFlow, stats *Stats, expired bool) {
	f.packets[0] = queuedPacket{}
	f.packets = f.packets[1:]
	q.count--
	stats.drops.Add(1)
	if expired {
		stats.queueExpired.Add(1)
	} else {
		stats.queueOverflow.Add(1)
	}
}
func (q *packetQueue) prune(now time.Time, stats *Stats) {
	active := q.flows[:0]
	for _, f := range q.flows {
		for q.maxAge > 0 && len(f.packets) > 0 && now.Sub(f.packets[0].at) > q.maxAge {
			q.dropHead(f, stats, true)
		}
		if len(f.packets) > 0 {
			active = append(active, f)
		}
	}
	clear(q.flows[len(active):])
	q.flows = active
}
func (q *packetQueue) push(packet []byte, now time.Time, stats *Stats) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.prune(now, stats)
	key := packetFlowKey(packet)
	// Drop from the longest flow, allowing a new short flow to enter a full queue.
	if q.count >= queueSize {
		largest := q.flows[0]
		for _, f := range q.flows {
			if len(f.packets) > len(largest.packets) {
				largest = f
			}
		}
		q.dropHead(largest, stats, false)
		q.prune(now, stats)
	}
	var flow *packetFlow
	for _, f := range q.flows {
		if f.key == key {
			flow = f
			break
		}
	}
	if flow == nil {
		flow = &packetFlow{key: key}
		q.flows = append(q.flows, flow)
	}
	flow.packets = append(flow.packets, queuedPacket{append([]byte(nil), packet...), now})
	q.count++
	storeMax(&stats.queuePeak, uint64(q.count))
	select {
	case q.ready <- struct{}{}:
	default:
	}
}
func (q *packetQueue) len() int { q.mu.Lock(); defer q.mu.Unlock(); return q.count }
func (q *packetQueue) pop(now time.Time, stats *Stats) []byte {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.prune(now, stats)
	for len(q.flows) > 0 {
		f := q.flows[0]
		if f.deficit < len(f.packets[0].data) {
			f.deficit += flowQuantum
			q.flows = append(q.flows[1:], f)
			continue
		}
		packet := f.packets[0]
		f.packets[0] = queuedPacket{}
		f.packets = f.packets[1:]
		f.deficit -= len(packet.data)
		q.count--
		if len(f.packets) == 0 {
			q.flows[0] = nil
			q.flows = q.flows[1:]
		}
		wait := now.Sub(packet.at)
		if wait > 0 {
			storeMax(&stats.queueWaitMax, uint64(wait))
		}
		return packet.data
	}
	return nil
}
