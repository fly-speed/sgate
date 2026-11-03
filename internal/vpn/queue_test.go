package vpn

import (
	"context"
	"encoding/binary"
	"sync"
	"testing"
	"time"
)

func queuePacket(port uint16, size int) []byte {
	p := make([]byte, size)
	p[0] = 0x45
	p[9] = 6
	binary.BigEndian.PutUint16(p[2:4], uint16(size))
	copy(p[12:20], []byte{10, 77, 0, 2, 8, 8, 8, 8})
	binary.BigEndian.PutUint16(p[20:22], port)
	binary.BigEndian.PutUint16(p[22:24], 443)
	return p
}
func TestVPNQueueShortFlowEntersFullBulkQueue(t *testing.T) {
	q, s := newPacketQueue(), &Stats{}
	now := time.Now()
	for n := 0; n < queueSize; n++ {
		q.push(queuePacket(1000, 1280), now, s)
	}
	short := queuePacket(2000, 60)
	q.push(short, now, s)
	found := false
	for n := 0; n < 3; n++ {
		p := q.pop(now.Add(time.Millisecond), s)
		if p != nil && packetFlowKey(p) == packetFlowKey(short) {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("short flow waited behind the entire bulk queue")
	}
	if s.Snapshot().QueueOverflow != 1 || s.Snapshot().QueuePeak != queueSize {
		t.Fatal(s.Snapshot())
	}
}
func TestVPNQueueByteFairnessAndPerFlowOrder(t *testing.T) {
	q, s := newPacketQueue(), &Stats{}
	now := time.Now()
	for n := 0; n < 32; n++ {
		large := queuePacket(1000, 1500)
		large[24] = byte(n)
		q.push(large, now, s)
		small := queuePacket(2000, 300)
		small[24] = byte(n)
		q.push(small, now, s)
	}
	bytes := map[uint16]int{}
	next := map[uint16]byte{}
	for n := 0; n < 30; n++ {
		p := q.pop(now, s)
		if p == nil {
			t.Fatal("queue emptied")
		}
		port := binary.BigEndian.Uint16(p[20:22])
		if p[24] != next[port] {
			t.Fatal("flow reordered")
		}
		next[port]++
		bytes[port] += len(p)
	}
	delta := bytes[1000] - bytes[2000]
	if delta < 0 {
		delta = -delta
	}
	if delta > flowQuantum {
		t.Fatal("byte unfair", bytes)
	}
}
func TestVPNQueueExpiryAndPacketOwnership(t *testing.T) {
	q, s := newPacketQueue(), &Stats{}
	now := time.Now()
	p := queuePacket(1000, 60)
	q.push(p, now, s)
	p[24] = 99
	if got := q.pop(now.Add(time.Millisecond), s); got[24] != 0 {
		t.Fatal("queued memory aliases caller")
	}
	q.push(p, now, s)
	if got := q.pop(now.Add(maxQueueAge+time.Nanosecond), s); got != nil || s.Snapshot().QueueExpired != 1 || q.len() != 0 {
		t.Fatal("stale packet sent", s.Snapshot())
	}
	q.push(p, now.Add(time.Second), s)
	if q.pop(now.Add(time.Second), s) == nil {
		t.Fatal("expiry blocked a fresh packet")
	}
}
func TestVPNQueueClassifierAndConcurrentBounds(t *testing.T) {
	a, b := queuePacket(1000, 60), queuePacket(2000, 60)
	if packetFlowKey(a) == packetFlowKey(b) {
		t.Fatal("TCP flows merged")
	}
	a[6] = 0x20
	b[7] = 1
	if packetFlowKey(a) != packetFlowKey(b) {
		t.Fatal("IP fragments classified using payload ports")
	}
	for n := 0; n < 20; n++ {
		packetFlowKey(a[:n])
	}
	q, s := newPacketQueue(), &Stats{}
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(port uint16) {
			defer wg.Done()
			for n := 0; n < 500; n++ {
				q.push(queuePacket(port, 60), time.Now(), s)
				if n%3 == 0 {
					q.pop(time.Now(), s)
				}
			}
		}(uint16(worker))
	}
	wg.Wait()
	if q.len() > queueSize || s.Snapshot().QueuePeak > queueSize {
		t.Fatal("queue unbounded")
	}
}
func TestVPNAdmissionWaitIsCancelableAndKeepsShortFlowSchedulable(t *testing.T) {
	flow := newDatagramFlow()
	flow.queued.Store(4)
	p := &peer{flow: flow, queue: newPacketQueue()}
	stats := &Stats{}
	now := time.Now()
	for n := 0; n < 30; n++ {
		p.queue.push(queuePacket(1000, 1280), now, stats)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- p.waitForAdmission(ctx, stats) }()
	await(t, "congestion admission", func() bool { return stats.admissionBlocked.Load() > 0 })
	select {
	case <-done:
		t.Fatal("admission ignored pending QUIC packets")
	default:
	}
	p.queue.push(queuePacket(2000, 60), now, stats)
	flow.sent.Add(1)
	flow.signal()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	found := false
	for n := 0; n < 3; n++ {
		if b := p.queue.pop(now, stats); binary.BigEndian.Uint16(b[20:22]) == 2000 {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("short flow was stranded while admission waited")
	}
	flow.queued.Store(flow.sent.Load() + 4)
	go func() { done <- p.waitForAdmission(ctx, stats) }()
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatal(err)
	}
}

func TestVPNFairQueueContinuousDuplexFragmentedPackets(t *testing.T) {
	server, cfg, remote := fixture(t, 4)
	local, _, _ := startClient(t, cfg)
	await(t, "authenticated VPN", func() bool { return cfg.Stats.Snapshot().Active == 1 })
	// Exceed the library's original 32-DATAGRAM FIFO repeatedly. Batches stay
	// below the intentional application queue limit, and consume both directions.
	for batch := 0; batch < 16; batch++ {
		out, in := make([][]byte, 8), make([][]byte, 8)
		for n := 0; n < 8; n++ {
			out[n] = ipPacket("10.77.0.2", "8.8.8.8", 1280)
			in[n] = ipPacket("8.8.8.8", "10.77.0.2", 1280)
			// A single inner flow in each direction verifies packet ordering.
			for _, p := range [][]byte{out[n], in[n]} {
				p[6] = 0
				p[7] = 0
				binary.BigEndian.PutUint16(p[20:22], 1000)
				binary.BigEndian.PutUint16(p[22:24], 443)
			}
			local.out <- out[n]
			remote.out <- in[n]
		}
		for n := 0; n < 8; n++ {
			receivePacket(t, remote, out[n])
			receivePacket(t, local, in[n])
		}
	}
	if cfg.Stats.Snapshot().Drops != 0 || server.cfg.Stats.Snapshot().Drops != 0 {
		t.Fatal("uncongested duplex dropped packets", cfg.Stats.Snapshot(), server.cfg.Stats.Snapshot())
	}
	server.mu.Lock()
	p := server.peers[cfg.ClientIP]
	server.mu.Unlock()
	if p.flow.pending() > 5 {
		t.Fatal("QUIC queue exceeded admission bound")
	}
}
