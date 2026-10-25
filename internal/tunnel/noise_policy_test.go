package tunnel

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func TestNoisePolicyClassification(t *testing.T) {
	now := time.Now()
	s := &session{streams: make(map[uint64]*stream)}
	options, err := (Options{}).defaults()
	if err != nil || options.NoiseIdleAfter != time.Second {
		t.Fatal(options, err)
	}
	if _, err := (Options{NoiseIdleAfter: -1}).defaults(); err == nil {
		t.Fatal("accepted negative idle threshold")
	}
	for mask := 0; mask < 8; mask++ {
		options.NoiseSuppressEmpty = mask&1 != 0
		options.NoiseSuppressTraffic = mask&2 != 0
		options.NoiseSuppressIdle = mask&4 != 0
		for _, state := range []noiseActivity{noiseEmpty, noiseTraffic, noiseIdle} {
			s.streams = make(map[uint64]*stream)
			s.lastBusiness = now
			want := !options.NoiseSuppressEmpty
			if state != noiseEmpty {
				s.streams[1] = &stream{}
				want = !options.NoiseSuppressTraffic
			}
			if state == noiseIdle {
				s.lastBusiness = now.Add(-options.NoiseIdleAfter)
				want = !options.NoiseSuppressIdle
			}
			if s.activityLocked(now, options.NoiseIdleAfter) != state || s.noiseAllowed(options, now) != want {
				t.Fatalf("mask=%d state=%d", mask, state)
			}
		}
	}
	s.lastBusiness = time.Time{}
	if s.activityLocked(now, time.Second) != noiseIdle {
		t.Fatal("new unused stream should be idle")
	}
}

// 每个策略在真实WS/WSS的两个发送方向验证，不能仅以配置快照或定时器次数代替实际帧。
func TestNoisePolicyLiveWire(t *testing.T) {
	for _, tlsMode := range []bool{false, true} {
		for _, clientWriter := range []bool{false, true} {
			for _, random := range []bool{false, true} {
				t.Run(fmt.Sprintf("tls=%t/client=%t/random=%t", tlsMode, clientWriter, random), func(t *testing.T) {
					value := LiveConfig{NoiseInterval: 10 * time.Millisecond, NoiseMinBytes: 1024, NoiseMaxBytes: 4096,
						NoiseSuppressTraffic: true, NoiseSuppressEmpty: true, NoiseIdleAfter: 40 * time.Millisecond}
					if random {
						value.NoiseInterval = 0
						value.NoiseIntervalMin = 10 * time.Millisecond
						value.NoiseIntervalMax = 20 * time.Millisecond
					}
					live, err := NewLiveSettings(value)
					if err != nil {
						t.Fatal(err)
					}
					stats := NewStats()
					s, peer, crypt := noiseWirePair(t, tlsMode, clientWriter, Options{Live: live, Stats: stats})
					var noise, noiseBytes, data atomic.Int64
					minBytes, maxBytes := value.NoiseMinBytes, value.NoiseMaxBytes
					readerDone := make(chan error, 1)
					go func() {
						for {
							_, sealed, err := peer.ReadMessage()
							if err != nil {
								readerDone <- err
								return
							}
							plain, err := crypt.open(sealed)
							if err != nil {
								readerDone <- err
								return
							}
							f, err := decodeFrame(plain)
							if err != nil {
								readerDone <- err
								return
							}
							if f.op == opNoise {
								if len(f.data) < minBytes || len(f.data) > maxBytes {
									readerDone <- fmt.Errorf("noise size outside configured bounds: %d", len(f.data))
									return
								}
								if bytes.Count(f.data, f.data[:1]) == len(f.data) {
									readerDone <- fmt.Errorf("noise contains only fixed byte %02x", f.data[0])
									return
								}
								noiseBytes.Add(int64(len(f.data)))
								noise.Add(1)
							}
							if f.op == opData {
								data.Add(1)
							}
						}
					}()
					t.Cleanup(func() { peer.Close(); <-readerDone })
					stopped := func(label string) {
						t.Helper()
						// 已经开始的一个小帧允许完成；等待后要求计数严格稳定。
						time.Sleep(40 * time.Millisecond)
						before := noise.Load()
						time.Sleep(80 * time.Millisecond)
						if noise.Load() != before {
							t.Fatalf("%s still emitted noise: %d -> %d", label, before, noise.Load())
						}
					}
					progressed := func(label string) {
						t.Helper()
						before := noise.Load()
						waitFor(t, label, func() bool { return noise.Load() >= before+3 })
					}
					update := func() {
						t.Helper()
						if err := live.Update(value); err != nil {
							t.Fatal(err)
						}
					}
					stopped("empty disabled")
					a, b := net.Pipe()
					t.Cleanup(func() { b.Close() })
					s.mu.Lock()
					s.streams[1] = &stream{conn: a}
					s.signalLocked()
					s.mu.Unlock()
					progressed("idle enabled")
					produceDone, produceStopped := make(chan struct{}), make(chan struct{})
					go func() {
						defer close(produceStopped)
						ticker := time.NewTicker(5 * time.Millisecond)
						defer ticker.Stop()
						for {
							select {
							case <-produceDone:
								return
							case <-ticker.C:
								if s.enqueue(opData, 1, []byte("application-data")) != nil {
									return
								}
							}
						}
					}()
					producing := true
					t.Cleanup(func() {
						if producing {
							close(produceDone)
							<-produceStopped
						}
					})
					waitFor(t, "business traffic started", func() bool { return data.Load() >= 5 })
					stopped("busy disabled")
					value.NoiseSuppressTraffic = false
					update()
					progressed("busy enabled")
					value.NoiseSuppressTraffic = true
					value.NoiseSuppressIdle = true
					update()
					stopped("all scenarios disabled")
					close(produceDone)
					<-produceStopped
					producing = false
					stopped("idle disabled")
					value.NoiseSuppressIdle = false
					update()
					progressed("idle re-enabled")
					s.drop(1)
					stopped("last business connection closed")
					value.NoiseSuppressEmpty = false
					update()
					progressed("empty enabled")
					value.NoiseInterval = 0
					value.NoiseIntervalMin = 0
					value.NoiseIntervalMax = 0
					update()
					stopped("global interval disabled")
					if got, want := stats.Snapshot().NoiseTX, uint64(noiseBytes.Load()); got != want {
						t.Fatalf("noise statistics differ from decrypted wire bytes: got=%d want=%d", got, want)
					}
					t.Logf("actual wire noise=%d business=%d", noise.Load(), data.Load())
				})
			}
		}
	}
}

func TestNoisePolicyRealStreamsAndReconnect(t *testing.T) {
	for _, mode := range []string{"", "tls"} {
		t.Run("protocol="+mode, func(t *testing.T) {
			noiseRecoveryResourceCheck(t)
			opts := testOptions()
			opts.NoiseIntervalMin, opts.NoiseIntervalMax = 10*time.Millisecond, 20*time.Millisecond
			opts.NoiseIdleAfter = 50 * time.Millisecond
			opts.NoiseSuppressTraffic, opts.NoiseSuppressEmpty = true, true
			f := newRecoveryFixtureOptions(t, 2*time.Minute, 2*time.Minute, mode, opts)
			client, remote := f.clientSession(), f.remoteSession()
			time.Sleep(100 * time.Millisecond)
			if recoveryNoiseSent(client) != 0 || recoveryNoiseSent(remote) != 0 {
				t.Fatal("noise with no business streams")
			}
			c := f.dial()
			waitFor(t, "idle streams emit in both directions", func() bool {
				return recoveryNoiseSent(client) >= 3 && recoveryNoiseSent(remote) >= 3
			})
			busy := func(stage string) {
				t.Helper()
				var before [2]uint64
				for i := 0; i < 24; i++ {
					payload := []byte(fmt.Sprintf("%s/record-%d", stage, i))
					if err := exchange(c, payload); err != nil {
						t.Fatal(err)
					}
					if i == 8 {
						before = [2]uint64{recoveryNoiseSent(client), recoveryNoiseSent(remote)}
					}
					time.Sleep(5 * time.Millisecond)
				}
				if recoveryNoiseSent(client) > before[0]+1 || recoveryNoiseSent(remote) > before[1]+1 {
					t.Fatal("noise continued during sustained application traffic")
				}
			}
			busy("before-reconnect")
			before := [2]uint64{recoveryNoiseSent(client), recoveryNoiseSent(remote)}
			waitFor(t, "idle gap resumes both directions", func() bool {
				return recoveryNoiseSent(client) >= before[0]+3 && recoveryNoiseSent(remote) >= before[1]+3
			})
			f.disconnect()
			// 此时保留业务连接，但旧传输的噪音时钟已随写协程停止。
			f.blocked.Store(false)
			waitFor(t, "reattached retained session", f.attached)
			if f.remoteSession() != remote || f.clientSession() != client {
				t.Fatal("reconnect replaced retained session")
			}
			busy("after-reconnect")
			c.Close()
			waitFor(t, "both directions release streams and replay cache", func() bool {
				for _, s := range []*session{client, remote} {
					s.mu.Lock()
					clean := len(s.streams) == 0 && s.pendingBytes == 0
					s.mu.Unlock()
					if !clean {
						return false
					}
				}
				return true
			})
			time.Sleep(50 * time.Millisecond)
			stopped := [2]uint64{recoveryNoiseSent(client), recoveryNoiseSent(remote)}
			time.Sleep(80 * time.Millisecond)
			if recoveryNoiseSent(client) != stopped[0] || recoveryNoiseSent(remote) != stopped[1] {
				t.Fatal("noise continued after last business stream closed")
			}
		})
	}
}

// 单向业务也必须令接收端处于忙碌，不能只用本端发送计数判断。
func TestNoisePolicyOneWayTraffic(t *testing.T) {
	for _, mode := range []string{"", "tls"} {
		for _, uplink := range []bool{false, true} {
			t.Run(fmt.Sprintf("protocol=%s/uplink=%t", mode, uplink), func(t *testing.T) {
				noiseRecoveryResourceCheck(t)
				start, hold := make(chan struct{}), make(chan struct{})
				backendDone := make(chan error, 1)
				transfer := func(c net.Conn, write bool, observe func(int)) error {
					for i := 0; i < 32; i++ {
						record := []byte(fmt.Sprintf("%064d", i))
						if write {
							if _, err := io.Copy(c, bytes.NewReader(record)); err != nil {
								return err
							}
							time.Sleep(5 * time.Millisecond)
						} else {
							got := make([]byte, len(record))
							if _, err := io.ReadFull(c, got); err != nil {
								return err
							}
							if !bytes.Equal(got, record) {
								return fmt.Errorf("lost or repeated record %d", i)
							}
						}
						if observe != nil {
							observe(i)
						}
					}
					return nil
				}
				opts := testOptions()
				opts.NoiseIntervalMin, opts.NoiseIntervalMax = 10*time.Millisecond, 20*time.Millisecond
				opts.NoiseIdleAfter = 50 * time.Millisecond
				opts.NoiseSuppressTraffic, opts.NoiseSuppressEmpty = true, true
				f := newRecoveryFixtureBackend(t, 2*time.Minute, 2*time.Minute, mode, opts, func(c net.Conn) {
					<-start
					backendDone <- transfer(c, !uplink, nil)
					<-hold
				})
				started := false
				t.Cleanup(func() {
					if !started {
						close(start)
					}
					close(hold)
				})
				c := f.dial()
				client, remote := f.clientSession(), f.remoteSession()
				waitFor(t, "idle before one-way traffic", func() bool { return recoveryNoiseSent(client) >= 2 && recoveryNoiseSent(remote) >= 2 })
				close(start)
				started = true
				var before [2]uint64
				if err := transfer(c, uplink, func(i int) {
					if i == 8 {
						before = [2]uint64{recoveryNoiseSent(client), recoveryNoiseSent(remote)}
					}
				}); err != nil {
					t.Fatal(err)
				}
				if err := <-backendDone; err != nil {
					t.Fatal(err)
				}
				if recoveryNoiseSent(client) > before[0]+1 || recoveryNoiseSent(remote) > before[1]+1 {
					t.Fatal("receive-only side did not suppress active traffic noise")
				}
				before = [2]uint64{recoveryNoiseSent(client), recoveryNoiseSent(remote)}
				waitFor(t, "one-way gap becomes idle again", func() bool {
					return recoveryNoiseSent(client) >= before[0]+2 && recoveryNoiseSent(remote) >= before[1]+2
				})
			})
		}
	}
}
