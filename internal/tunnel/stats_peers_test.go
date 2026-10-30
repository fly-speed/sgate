package tunnel

import (
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

type peerTestConn struct {
	net.Conn
	remote net.Addr
}

func (c peerTestConn) RemoteAddr() net.Addr { return c.remote }

func TestTrafficPeersAggregateBytesAndRelease(t *testing.T) {
	stats := NewStats()
	for _, ip := range []string{"127.0.0.1", "127.0.0.2", "127.0.0.1"} {
		a, b := net.Pipe()
		c := stats.Conn(peerTestConn{Conn: a, remote: &net.TCPAddr{IP: net.ParseIP(ip), Port: 80}})
		done := make(chan error, 1)
		go func() {
			_, err := b.Write([]byte("in"))
			if err == nil {
				buf := make([]byte, 3)
				_, err = io.ReadFull(b, buf)
			}
			done <- err
		}()
		buf := make([]byte, 2)
		if _, err := io.ReadFull(c, buf); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Write([]byte("out")); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		stats.peerPayload(c, "businessTX", 3)
		stats.peerPayload(c, "noiseRX", 2)
		c.Close()
		c.Close()
		b.Close()
	}
	snapshot := stats.Snapshot()
	if len(snapshot.ByIP) != 2 || snapshot.TX != 9 || snapshot.RX != 6 || snapshot.Connections != 0 {
		t.Fatal(snapshot)
	}
	p := snapshot.ByIP[0]
	if p.IP != "127.0.0.1" || p.TX != 6 || p.RX != 4 || p.BusinessTX != 6 || p.NoiseRX != 4 || p.Connections != 0 {
		t.Fatal(p)
	}
	stats.peersMu.Lock()
	for _, peer := range stats.peers {
		peer.idle = time.Now().Add(-2 * trafficPeerRetention)
	}
	stats.peersMu.Unlock()
	if len(stats.Snapshot().ByIP) != 0 {
		t.Fatal("closed peer counters retained")
	}
}

func TestTrafficPeersBoundedConcurrentCardinality(t *testing.T) {
	stats := NewStats()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for n := 0; n < 200; n++ {
				ip := net.ParseIP(fmt.Sprintf("10.%d.%d.%d", worker, n/250, n%250+1))
				p := stats.openPeer(&net.TCPAddr{IP: ip, Port: 1})
				p.tx.Add(1)
				stats.Snapshot()
				stats.peersMu.Lock()
				p.active--
				p.idle = time.Now()
				stats.peersMu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	peers := stats.peerSnapshots()
	if len(peers) > maxTrafficPeers+1 {
		t.Fatal("unbounded peer statistics", len(peers))
	}
	var total uint64
	for _, p := range peers {
		total += p.TX
		if p.Connections != 0 {
			t.Fatal("peer active count leaked")
		}
	}
	if total != 1600 {
		t.Fatal("overflow lost bytes", total)
	}
}

func TestPeerTrafficWSWSSPayloadAndReconnect(t *testing.T) {
	for _, mode := range []string{"", "tls"} {
		t.Run("protocol="+mode, func(t *testing.T) {
			serverStats, clientStats := NewStats(), NewStats()
			t.Cleanup(func() {
				for _, stats := range []*Stats{serverStats, clientStats} {
					if snapshot := stats.Snapshot(); snapshot.Connections != 0 {
						t.Error("TCP connections retained", snapshot)
					}
					for _, peer := range stats.Snapshot().ByIP {
						if peer.Connections != 0 {
							t.Error("peer connections retained", peer)
						}
					}
				}
			})
			opts := testOptions()
			opts.Stats = serverStats
			opts.NoiseIntervalMin, opts.NoiseIntervalMax = time.Millisecond, 3*time.Millisecond
			f := newRecoveryFixtureOptions(t, time.Second, time.Second, mode, opts, func(cfg *ClientConfig) { cfg.Options.Stats = clientStats })
			app := f.dial()
			defer app.Close()
			if err := exchange(app, []byte("per-IP duplex business")); err != nil {
				t.Fatal(err)
			}
			assertPayload := func() {
				t.Helper()
				waitFor(t, "per-IP business and noise counters", func() bool {
					for _, stats := range []*Stats{serverStats, clientStats} {
						snapshot := stats.Snapshot()
						if len(snapshot.ByIP) != 1 {
							return false
						}
						peer := snapshot.ByIP[0]
						if peer.BusinessTX == 0 || peer.BusinessRX == 0 || peer.NoiseTX == 0 || peer.NoiseRX == 0 || peer.TX == 0 || peer.RX == 0 {
							return false
						}
						if peer.BusinessTX != snapshot.BusinessTX || peer.BusinessRX != snapshot.BusinessRX {
							return false
						}
					}
					return true
				})
			}
			assertPayload()
			f.disconnect()
			f.blocked.Store(false)
			waitFor(t, "reconnected per-IP tunnel", f.attached)
			if err := exchange(app, []byte("after reconnect")); err != nil {
				t.Fatal(err)
			}
			assertPayload()
		})
	}
}
