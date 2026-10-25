package tunnel

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// 按实际解密后的线帧核对 10 MiB 总量、等待间隔、业务穿插和心跳。
func TestNoiseTenMiBBurstScheduling(t *testing.T) {
	for _, tlsMode := range []bool{false, true} {
		for _, random := range []bool{false, true} {
			name := "ws/fixed"
			if tlsMode {
				name = "wss/fixed"
			}
			if random {
				name = strings.Replace(name, "fixed", "random", 1)
			}
			t.Run(name, func(t *testing.T) {
				connections := make(chan *websocket.Conn, 1)
				h := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
					if err == nil {
						connections <- c
					}
				}))
				if tlsMode {
					h.StartTLS()
				} else {
					h.Start()
				}
				defer h.Close()
				dialer := *websocket.DefaultDialer
				if tlsMode {
					dialer.TLSClientConfig = h.Client().Transport.(*http.Transport).TLSClientConfig
				}
				peer, _, err := dialer.Dial("ws"+strings.TrimPrefix(h.URL, "http"), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer peer.Close()
				writer := <-connections
				defer writer.Close()
				opts := testOptions()
				opts.Stats = NewStats()
				opts.NoiseInterval = 80 * time.Millisecond
				opts.NoiseMinBytes, opts.NoiseMaxBytes = maxNoiseBytes, maxNoiseBytes
				opts.Heartbeat, opts.DialTimeout, opts.WriteTimeout = 20*time.Millisecond, 10*time.Millisecond, time.Second
				opts.HeartbeatTimeout = 3 * time.Second
				if random {
					opts.NoiseInterval = 0
					opts.NoiseIntervalMin, opts.NoiseIntervalMax = 80*time.Millisecond, 100*time.Millisecond
				}
				opts, err = opts.defaults()
				if err != nil {
					t.Fatal(err)
				}
				s := newSession(context.Background(), "", opts)
				defer s.close()
				key := bytes.Repeat([]byte{9}, 32)
				tx, rx := newCodec(key, "session", "c", "s", false), newCodec(key, "session", "c", "s", true)
				done, finished := make(chan struct{}), make(chan error, 1)
				started := time.Now()
				go func() { finished <- s.writeTransport(writer, tx, done, nil) }()
				defer func() {
					close(done)
					writer.Close()
					select {
					case <-finished:
					case <-time.After(2 * time.Second):
						t.Error("large noise writer did not stop")
					}
				}()
				pings := 0
				peer.SetPingHandler(func(string) error { pings++; return nil })
				peer.SetReadDeadline(time.Now().Add(10 * time.Second))
				total, bursts := 0, 0
				gap := make(chan time.Duration, 1)
				go func() {
					ticker := time.NewTicker(time.Millisecond)
					defer ticker.Stop()
					var completed time.Time
					for {
						select {
						case <-done:
							return
						case now := <-ticker.C:
							sent := opts.Stats.Snapshot().NoiseTX
							if sent == maxNoiseBytes && completed.IsZero() {
								completed = now
							}
							if sent > maxNoiseBytes {
								gap <- now.Sub(completed)
								return
							}
						}
					}
				}()
				inserted, delivered := false, false
				firstPings, heartbeatDuringBurst := -1, false
				for bursts < 2 {
					_, sealed, err := peer.ReadMessage()
					if err != nil {
						t.Fatal(err)
					}
					plain, err := rx.open(sealed)
					if err != nil {
						t.Fatal(err)
					}
					f, err := decodeFrame(plain)
					if err != nil {
						t.Fatal(err)
					}
					if f.op == opData {
						if delivered || !bytes.Equal(f.data, []byte("business-during-noise")) {
							t.Fatal("business loss or duplication")
						}
						delivered = true
						if bursts == 0 && total >= maxNoiseBytes {
							t.Fatal("business waited for whole noise burst")
						}
						continue
					}
					if f.op != opNoise {
						continue
					}
					if total == 0 && bursts == 0 {
						firstPings = pings
						earliest := started.Add(70 * time.Millisecond)
						if time.Now().Before(earliest) {
							t.Fatal("configured noise wait was skipped")
						}
					}
					if len(f.data) > chunkSize {
						t.Fatal("oversized noise wire chunk")
					}
					total += len(f.data)
					if total > maxNoiseBytes {
						t.Fatal("noise burst exceeded 10 MiB")
					}
					if !inserted {
						if err := s.enqueue(opData, 42, []byte("business-during-noise")); err != nil {
							t.Fatal(err)
						}
						inserted = true
					}
					if total == maxNoiseBytes {
						if bursts == 0 {
							heartbeatDuringBurst = pings > firstPings
						}
						bursts++
						total = 0
					}
					time.Sleep(time.Millisecond)
				}
				if !delivered || pings < 2 || !heartbeatDuringBurst {
					t.Fatalf("business/heartbeat stalled: delivered=%t pings=%d", delivered, pings)
				}
				select {
				case wait := <-gap:
					if wait < 70*time.Millisecond {
						t.Fatalf("sender skipped configured wait: %s", wait)
					}
				case <-time.After(time.Second):
					t.Fatal("missing sender timing observation")
				}
				if sent := opts.Stats.Snapshot().NoiseTX; sent < 2*maxNoiseBytes || sent >= 3*maxNoiseBytes {
					t.Fatal("noise payload statistics mismatch", opts.Stats.Snapshot())
				}
				s.mu.Lock()
				defer s.mu.Unlock()
				if s.pendingBytes > frameHeader+len("business-during-noise") || len(s.pending) != 1 {
					t.Fatal("noise retained in replay cache")
				}
			})
		}
	}
}
