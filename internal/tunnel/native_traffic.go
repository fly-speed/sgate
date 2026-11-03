package tunnel

import (
	"io"
	"net"
	"sync"
	"time"
)

type nativeTraffic struct {
	stats *Stats
	peer  *peerTraffic
	once  sync.Once
}

func newNativeTraffic(stats *Stats, addr net.Addr) *nativeTraffic {
	t := &nativeTraffic{stats: stats}
	if stats != nil {
		stats.active.Add(1)
		t.peer = stats.openPeer(addr)
	}
	return t
}
func (t *nativeTraffic) tx(n int) {
	if t != nil && t.stats != nil {
		t.stats.tx.Add(uint64(n))
		t.peer.tx.Add(uint64(n))
	}
}
func (t *nativeTraffic) rx(n int) {
	if t != nil && t.stats != nil {
		t.stats.rx.Add(uint64(n))
		t.peer.rx.Add(uint64(n))
	}
}
func (t *nativeTraffic) close() {
	if t == nil || t.stats == nil {
		return
	}
	t.once.Do(func() {
		t.stats.active.Add(-1)
		t.stats.peersMu.Lock()
		t.peer.active--
		if t.peer.active == 0 {
			t.peer.idle = time.Now()
		}
		t.stats.peersMu.Unlock()
	})
}

type nativeIO struct {
	io.ReadWriter
	traffic *nativeTraffic
}

func (c nativeIO) Read(p []byte) (int, error) {
	n, err := c.ReadWriter.Read(p)
	c.traffic.rx(n)
	return n, err
}
func (c nativeIO) Write(p []byte) (int, error) {
	n, err := c.ReadWriter.Write(p)
	c.traffic.tx(n)
	return n, err
}
