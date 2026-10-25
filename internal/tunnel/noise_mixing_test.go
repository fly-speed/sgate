package tunnel

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// 在真实WS/WSS上解密观察，不能用定时器触发次数代替实际成功发送的噪音。
func noiseWirePair(t *testing.T, tlsMode, clientWriter bool, opts Options) (*session, *websocket.Conn, *codec) {
	t.Helper()
	accepted := make(chan *websocket.Conn, 1)
	h := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err == nil {
			accepted <- c
		}
	}))
	if tlsMode {
		h.StartTLS()
	} else {
		h.Start()
	}
	t.Cleanup(h.Close)
	dialer := *websocket.DefaultDialer
	if tlsMode {
		dialer.TLSClientConfig = h.Client().Transport.(*http.Transport).TLSClientConfig
	}
	down, _, err := dialer.Dial("ws"+strings.TrimPrefix(h.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	up := <-accepted
	writer, peer := up, down
	if clientWriter {
		writer, peer = down, up
	}
	opts, err = opts.defaults()
	if err != nil {
		t.Fatal(err)
	}
	s := newSession(context.Background(), "", opts)
	key := bytes.Repeat([]byte{23}, 32)
	tx, rx := newCodec(key, "session", "c", "s", clientWriter), newCodec(key, "session", "c", "s", !clientWriter)
	done, finished := make(chan struct{}), make(chan error, 1)
	go func() { finished <- s.writeTransport(writer, tx, done, nil) }()
	t.Cleanup(func() {
		close(done)
		s.close()
		writer.Close()
		peer.Close()
		select {
		case <-finished:
		case <-time.After(2 * time.Second):
			t.Error("wire writer did not stop")
		}
	})
	return s, peer, rx
}

func TestNoiseNotPostponedByUnrelatedLiveUpdates(t *testing.T) {
	for _, tlsMode := range []bool{false, true} {
		for _, clientWriter := range []bool{false, true} {
			for _, kind := range []string{"reconnect", "size"} {
				name := fmt.Sprintf("tls=%t/client-writer=%t/%s", tlsMode, clientWriter, kind)
				t.Run(name, func(t *testing.T) {
					value := LiveConfig{NoiseInterval: 60 * time.Millisecond, NoiseMinBytes: 1024, NoiseMaxBytes: 4096}
					live, err := NewLiveSettings(value)
					if err != nil {
						t.Fatal(err)
					}
					s, peer, rx := noiseWirePair(t, tlsMode, clientWriter, Options{Live: live})
					var noise, data atomic.Int64
					readDone := make(chan error, 1)
					go func() {
						for {
							_, p, err := peer.ReadMessage()
							if err != nil {
								readDone <- err
								return
							}
							p, err = rx.open(p)
							if err != nil {
								readDone <- err
								return
							}
							f, err := decodeFrame(p)
							if err != nil {
								readDone <- err
								return
							}
							if f.op == opNoise {
								noise.Add(1)
							}
							if f.op == opData {
								data.Add(1)
							}
						}
					}()
					t.Cleanup(func() { peer.Close(); <-readDone })
					// 修改周期小于发送周期。重连和大小更改都不应不断推迟既定的发送时间。
					until := time.Now().Add(360 * time.Millisecond)
					index := 0
					for time.Now().Before(until) {
						index++
						next, _ := live.Snapshot()
						if kind == "reconnect" {
							next.ReconnectMin = time.Duration(index+1) * time.Millisecond
						} else {
							next.NoiseMaxBytes = 4096 + index
						}
						if err := live.Update(next); err != nil {
							t.Fatal(err)
						}
						if err := s.enqueue(opData, 1, []byte("application-only")); err != nil {
							t.Fatal(err)
						}
						time.Sleep(5 * time.Millisecond)
					}
					if data.Load() < 2 || noise.Load() < 2 {
						t.Fatalf("busy wire: application=%d noise=%d; unrelated updates postponed noise", data.Load(), noise.Load())
					}
					t.Logf("application=%d noise=%d during continuous %s edits", data.Load(), noise.Load(), kind)
				})
			}
		}
	}
}

// 每个阶段记录独立WebSocket上的实际帧，而不是整个进程的累计带宽。
type noisePhaseCount struct {
	Noise         int   `json:"noise_frames"`
	NoiseBytes    int   `json:"noise_bytes"`
	Data          int   `json:"data_frames"`
	DataBytes     int   `json:"data_bytes"`
	MaxNoiseGapMS int64 `json:"max_noise_gap_ms"`
	lastNoise     time.Time
}

func (c *noisePhaseCount) add(f frame) {
	if f.op == opNoise {
		now := time.Now()
		if !c.lastNoise.IsZero() {
			c.MaxNoiseGapMS = max(c.MaxNoiseGapMS, now.Sub(c.lastNoise).Milliseconds())
		}
		c.lastNoise = now
		c.Noise++
		c.NoiseBytes += len(f.data)
	} else if f.op == opData {
		c.Data++
		c.DataBytes += len(f.data)
	}
}

func saveNoiseObservation(t *testing.T, name string, value any) {
	t.Helper()
	directory := os.Getenv("SGATE_NOISE_OBSERVATION_DIR")
	if directory == "" {
		return
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(directory, name+".json"), append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}

// 从中继逐连接观察密文，实际接收端仍处理原始密文；回显必须逐字节一致。
func TestNoiseMixingPerConnectionBusyAndIdle(t *testing.T) {
	for _, tlsMode := range []bool{false, true} {
		t.Run(fmt.Sprintf("tls=%t", tlsMode), func(t *testing.T) {
			backend, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			var backendWG sync.WaitGroup
			var backendActive atomic.Int64
			var backendConns sync.Map
			backendWG.Add(1)
			go func() {
				defer backendWG.Done()
				for {
					c, err := backend.Accept()
					if err != nil {
						return
					}
					backendConns.Store(c, true)
					backendActive.Add(1)
					backendWG.Add(1)
					go func() {
						defer backendWG.Done()
						defer backendActive.Add(-1)
						defer backendConns.Delete(c)
						defer c.Close()
						_, _ = io.Copy(c, c)
					}()
				}
			}()
			t.Cleanup(func() {
				backend.Close()
				backendConns.Range(func(k, _ any) bool { k.(net.Conn).Close(); return true })
				backendWG.Wait()
			})
			key := bytes.Repeat([]byte{29}, 32)
			opts := testOptions()
			opts.BufferBytes = 1024 * 1024
			opts.NoiseIntervalMin, opts.NoiseIntervalMax = 20*time.Millisecond, 40*time.Millisecond
			opts.NoiseMinBytes, opts.NoiseMaxBytes = 1024, 4096
			server, err := NewServer(ServerConfig{Key: key, Password: testPassword, Target: backend.Addr().String(), Options: opts})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(server.Close)
			startHTTP := func(handler http.Handler) *httptest.Server {
				h := httptest.NewUnstartedServer(handler)
				if tlsMode {
					h.StartTLS()
				} else {
					h.Start()
				}
				t.Cleanup(h.Close)
				return h
			}
			upstream := startHTTP(server)
			observer := &configNoiseObserver{
				key: key, target: "ws" + strings.TrimPrefix(upstream.URL, "http") + "/tunnel",
				pairs: make(map[*websocket.Conn]*websocket.Conn), sessions: make(map[string]bool),
				expected: [2]Options{opts, opts}, sizes: [2]map[int]bool{{}, {}}, byGeneration: make(map[int][2]int),
			}
			if tlsMode {
				observer.tlsConfig = upstream.Client().Transport.(*http.Transport).TLSClientConfig
			}
			var phaseMu sync.Mutex
			phase := "warmup"
			phases := make(map[string]map[string]*[2]noisePhaseCount)
			observer.onFrame = func(direction int, id string, f frame) {
				phaseMu.Lock()
				defer phaseMu.Unlock()
				if phases[phase] == nil {
					phases[phase] = make(map[string]*[2]noisePhaseCount)
				}
				if phases[phase][id] == nil {
					phases[phase][id] = &[2]noisePhaseCount{}
				}
				phases[phase][id][direction].add(f)
			}
			relay := startHTTP(observer)
			t.Cleanup(func() { observer.disconnect(); observer.wg.Wait() })
			var apps []net.Conn
			for i := 0; i < 3; i++ {
				local, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { local.Close() })
				cfg := ClientConfig{Key: key, Password: testPassword, URL: "ws" + strings.TrimPrefix(relay.URL, "http") + "/tunnel", Options: opts}
				if tlsMode {
					cfg.TLSConfig = relay.Client().Transport.(*http.Transport).TLSClientConfig
				}
				client, err := NewClient(cfg)
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan error, 1)
				go func() { done <- client.Run(ctx, local) }()
				t.Cleanup(func() {
					cancel()
					select {
					case <-done:
					case <-time.After(3 * time.Second):
						t.Error("client did not stop")
					}
				})
				for j := 0; j < 4; j++ {
					c, err := net.DialTimeout("tcp", local.Addr().String(), time.Second)
					if err != nil {
						t.Fatal(err)
					}
					apps = append(apps, c)
					t.Cleanup(func() { c.Close() })
					c.SetDeadline(time.Now().Add(10 * time.Second))
					if err := exchange(c, []byte(fmt.Sprintf("mapping=%d/stream=%d", i, j))); err != nil {
						t.Fatal(err)
					}
				}
			}
			setPhase := func(name string) { phaseMu.Lock(); phase = name; phaseMu.Unlock() }
			setPhase("busy")
			var work sync.WaitGroup
			errors := make(chan error, len(apps))
			until := time.Now().Add(800 * time.Millisecond)
			for i, c := range apps {
				work.Add(1)
				go func(i int, c net.Conn) {
					defer work.Done()
					payload := bytes.Repeat([]byte(fmt.Sprintf("connection=%02d/", i)), 4096)
					var sequence uint64
					for time.Now().Before(until) {
						sequence++
						binary.BigEndian.PutUint64(payload[:8], sequence)
						if err := exchange(c, payload); err != nil {
							errors <- err
							return
						}
					}
				}(i, c)
			}
			work.Wait()
			close(errors)
			for err := range errors {
				t.Fatal(err)
			}
			setPhase("idle")
			time.Sleep(300 * time.Millisecond)
			for _, c := range apps {
				// 检查末尾没有重复业务数据，也没有泄露到应用端的噪音。
				c.SetReadDeadline(time.Now().Add(5 * time.Millisecond))
				var extra [1]byte
				n, err := c.Read(extra[:])
				if timeout, ok := err.(net.Error); n != 0 || !ok || !timeout.Timeout() {
					t.Fatalf("unexpected application tail: n=%d err=%v", n, err)
				}
				c.Close()
			}
			waitFor(t, "backend connections released", func() bool { return backendActive.Load() == 0 })
			waitFor(t, "server streams and cache released", func() bool {
				server.mu.Lock()
				defer server.mu.Unlock()
				for _, entry := range server.sessions {
					entry.session.mu.Lock()
					clean := len(entry.session.streams) == 0 && entry.session.pendingBytes == 0
					entry.session.mu.Unlock()
					if !clean {
						return false
					}
				}
				return true
			})
			// 两个方向的可靠序号均已确认，不能只检查服务端的缓存。
			waitFor(t, "both directions acknowledged", func() bool {
				observer.mu.Lock()
				defer observer.mu.Unlock()
				if len(observer.wire) != 3 {
					return false
				}
				for _, progress := range observer.wire {
					if progress.sent != progress.ack {
						return false
					}
				}
				return true
			})
			setPhase("closed-streams")
			time.Sleep(300 * time.Millisecond)
			phaseMu.Lock()
			defer phaseMu.Unlock()
			for _, name := range []string{"busy", "idle", "closed-streams"} {
				if len(phases[name]) != 3 {
					t.Fatalf("%s: observed %d connections, want 3", name, len(phases[name]))
				}
				for id, counts := range phases[name] {
					for direction, c := range counts {
						if c.Noise < 2 || (name == "busy" && c.Data == 0) || (name != "busy" && c.Data != 0) {
							t.Errorf("%s/%s/direction=%d: %+v", name, id, direction, c)
						}
						t.Logf("%s/%s/direction=%d: noise=%d data=%d max-noise-gap=%dms", name, id, direction, c.Noise, c.Data, c.MaxNoiseGapMS)
					}
				}
			}
			observer.mu.Lock()
			defer observer.mu.Unlock()
			if len(observer.errors) > 0 || observer.upgrades != 3 {
				t.Errorf("wire errors=%v upgrades=%d", observer.errors, observer.upgrades)
			}
			saveNoiseObservation(t, fmt.Sprintf("mixing-tls-%t", tlsMode), phases)
		})
	}
}
