package tunnel

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 在同一进程、同一可靠会话里反复切换场景，避免重新创建会话掩盖状态或资源残留。
func TestNoiseContinuousScenarioSwitch(t *testing.T) {
	for _, mode := range []string{"", "tls"} {
		t.Run("protocol="+mode, func(t *testing.T) {
			noiseRecoveryResourceCheck(t)
			r := newNoiseScenarioRun(t, mode)
			for round := 0; round < 3; round++ {
				t.Run(fmt.Sprintf("round=%d", round), func(t *testing.T) {
					r.value.NoiseIntervalMin = time.Duration(5+round*3) * time.Millisecond
					r.value.NoiseIntervalMax = time.Duration(15+round*5) * time.Millisecond
					r.value.NoiseMinBytes, r.value.NoiseMaxBytes = 1024*(round+1), 4096*(round+1)
					r.checkPolicy(t, noiseEmpty)
					conns := make([]net.Conn, 128)
					for i := range conns {
						conns[i] = r.f.dial()
					}
					stop := r.startBusiness(t, conns, round)
					r.checkPolicy(t, noiseTraffic)
					for i := 0; i < 2; i++ {
						r.reconnect(t)
					}
					stop()
					r.waitScene(t, noiseIdle)
					r.checkPolicy(t, noiseIdle)
					r.reconnect(t)
					r.checkQuietStats(t)
					for _, c := range conns {
						// 精确记录回显之后不能再出现重复字节或泄露给业务端的噪音。
						_ = c.SetReadDeadline(time.Now().Add(2 * time.Millisecond))
						var tail [1]byte
						n, err := c.Read(tail[:])
						if timeout, ok := err.(net.Error); n != 0 || !ok || !timeout.Timeout() {
							t.Fatalf("unexpected application tail: n=%d err=%v", n, err)
						}
						c.Close()
					}
					r.waitEmpty(t)
					r.reconnect(t)
					t.Logf("records=%d application-bytes=%d client=%+v server=%+v",
						r.records.Load(), r.businessBytes.Load(), r.stats[0].Snapshot(), r.stats[1].Snapshot())
				})
			}
			if got, want := r.f.backendBytes.Load(), r.businessBytes.Load(); got != want {
				t.Fatalf("backend received lost or duplicated business bytes: got=%d want=%d", got, want)
			}
			for _, stats := range r.stats {
				v := stats.Snapshot()
				if v.BusinessTX < uint64(r.businessBytes.Load()) || v.BusinessRX < uint64(r.businessBytes.Load()) {
					t.Fatal("business counters omitted delivered bytes", v)
				}
				if v.TX < v.NoiseTX+v.BusinessTX || v.RX < v.NoiseRX+v.BusinessRX {
					t.Fatal("TCP statistics omitted business or noise payload", v)
				}
			}
			r.f.cancel()
			r.f.stopServer()
			for _, s := range r.sessions {
				assertReleased(t, "scenario session, caches and writer timer released", s)
			}
			waitFor(t, "all scenario statistics references released", func() bool {
				for _, stats := range r.stats {
					snapshot := stats.Snapshot()
					v := snapshot.NoiseActivity
					if snapshot.Connections != 0 || v.Sessions != 0 || v.Tunnels != 0 || v.Streams != 0 || v.ReceiveBufferBytes != 0 || v.ReplayBufferBytes != 0 {
						return false
					}
				}
				return r.f.backendCount.active.Load() == 0 && r.f.localCount.active.Load() == 0
			})
		})
	}
}

type noiseScenarioRun struct {
	f             *recoveryFixture
	value         LiveConfig
	live          [2]*LiveSettings
	stats         [2]*Stats
	sessions      [2]*session
	businessBytes atomic.Int64
	records       atomic.Int64
}

func newNoiseScenarioRun(t *testing.T, mode string) *noiseScenarioRun {
	t.Helper()
	r := &noiseScenarioRun{value: LiveConfig{
		NoiseIntervalMin: 5 * time.Millisecond, NoiseIntervalMax: 15 * time.Millisecond,
		NoiseMinBytes: 1024, NoiseMaxBytes: 4096, NoiseIdleAfter: 200 * time.Millisecond,
		ReconnectMin: 10 * time.Millisecond, ReconnectMax: 30 * time.Millisecond,
	}}
	for i := range r.live {
		var err error
		r.live[i], err = NewLiveSettings(r.value)
		if err != nil {
			t.Fatal(err)
		}
		r.stats[i] = NewStats()
	}
	opts := testOptions()
	opts.Live, opts.Stats = r.live[1], r.stats[1]
	r.f = newRecoveryFixtureOptions(t, 2*time.Minute, 2*time.Minute, mode, opts, func(c *ClientConfig) {
		c.Options.Live, c.Options.Stats = r.live[0], r.stats[0]
	})
	r.sessions = [2]*session{r.f.clientSession(), r.f.remoteSession()}
	return r
}

func (r *noiseScenarioRun) update(t *testing.T) {
	t.Helper()
	for _, live := range r.live {
		if err := live.Update(r.value); err != nil {
			t.Fatal(err)
		}
	}
}

func (r *noiseScenarioRun) waitScene(t *testing.T, scene noiseActivity) {
	t.Helper()
	waitFor(t, fmt.Sprintf("both peers in scene %d", scene), func() bool {
		for _, stats := range r.stats {
			v := stats.Snapshot().NoiseActivity
			if v.Tunnels != 1 || (scene == noiseEmpty && v.Empty != 1) ||
				(scene == noiseTraffic && v.Traffic != 1) || (scene == noiseIdle && v.Idle != 1) {
				return false
			}
		}
		return true
	})
}

func (r *noiseScenarioRun) checkPolicy(t *testing.T, scene noiseActivity) {
	t.Helper()
	r.waitScene(t, scene)
	for _, enabled := range []bool{false, true} {
		r.value.NoiseSuppressEmpty, r.value.NoiseSuppressTraffic, r.value.NoiseSuppressIdle = true, true, true
		switch scene {
		case noiseEmpty:
			r.value.NoiseSuppressEmpty = !enabled
		case noiseTraffic:
			r.value.NoiseSuppressTraffic = !enabled
		case noiseIdle:
			r.value.NoiseSuppressIdle = !enabled
		}
		r.update(t)
		// 允许在线修改前已开始的小帧完成；之后要求禁用策略计数严格稳定。
		time.Sleep(80 * time.Millisecond)
		before := [2]TrafficSnapshot{r.stats[0].Snapshot(), r.stats[1].Snapshot()}
		if enabled {
			waitFor(t, "both peers receive enabled scene noise", func() bool {
				return r.stats[0].Snapshot().NoiseRX > before[0].NoiseRX && r.stats[1].Snapshot().NoiseRX > before[1].NoiseRX
			})
		} else {
			time.Sleep(100 * time.Millisecond)
			for i, stats := range r.stats {
				after := stats.Snapshot()
				if after.NoiseTX != before[i].NoiseTX || after.NoiseRX != before[i].NoiseRX {
					t.Fatalf("disabled scene %d emitted noise: before=%+v after=%+v", scene, before[i], after)
				}
			}
		}
	}
}

func (r *noiseScenarioRun) reconnect(t *testing.T) {
	t.Helper()
	r.f.disconnect()
	for _, s := range r.sessions {
		assertRecoveryNoiseStopped(t, s)
	}
	before := [2]uint64{r.stats[0].Snapshot().NoiseTX, r.stats[1].Snapshot().NoiseTX}
	time.Sleep(30 * time.Millisecond)
	for i, stats := range r.stats {
		if v := stats.Snapshot(); v.NoiseTX != before[i] || v.NoiseActivity.Tunnels != 0 {
			t.Fatal("offline writer or timer retained", v)
		}
	}
	r.f.blocked.Store(false)
	waitFor(t, "same session restored after scenario disconnect", func() bool {
		return r.f.remoteSession() == r.sessions[1] && r.f.clientSession() == r.sessions[0] &&
			r.stats[0].Snapshot().NoiseActivity.Tunnels == 1 && r.stats[1].Snapshot().NoiseActivity.Tunnels == 1
	})
}

func (r *noiseScenarioRun) startBusiness(t *testing.T, conns []net.Conn, round int) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errors := make(chan error, len(conns))
	var wg sync.WaitGroup
	for i, c := range conns {
		wg.Add(1)
		go func(i int, c net.Conn) {
			defer wg.Done()
			for seq := 0; ctx.Err() == nil; seq++ {
				payload := bytes.Repeat([]byte(fmt.Sprintf("round=%d/stream=%03d/seq=%08d;", round, i, seq)), 16)
				_ = c.SetDeadline(time.Now().Add(10 * time.Second))
				if err := exchange(c, payload); err != nil {
					errors <- fmt.Errorf("stream=%d seq=%d: %w", i, seq, err)
					return
				}
				r.businessBytes.Add(int64(len(payload)))
				r.records.Add(1)
				time.Sleep(5 * time.Millisecond)
			}
		}(i, c)
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			wg.Wait()
			close(errors)
			for err := range errors {
				t.Error(err)
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

// 在无断线的采样窗口精确比较发送/接收增量，避免将断线丢弃的噪音误认为统计错误。
func (r *noiseScenarioRun) checkQuietStats(t *testing.T) {
	t.Helper()
	freeze := func() [2]TrafficSnapshot {
		r.value.NoiseSuppressTraffic, r.value.NoiseSuppressIdle, r.value.NoiseSuppressEmpty = true, true, true
		r.update(t)
		time.Sleep(100 * time.Millisecond)
		return [2]TrafficSnapshot{r.stats[0].Snapshot(), r.stats[1].Snapshot()}
	}
	before := freeze()
	counts := [2]uint64{recoveryNoiseSent(r.sessions[0]), recoveryNoiseSent(r.sessions[1])}
	r.value.NoiseSuppressIdle = false
	r.update(t)
	waitFor(t, "statistics probe received noise", func() bool {
		return r.stats[0].Snapshot().NoiseRX > before[0].NoiseRX && r.stats[1].Snapshot().NoiseRX > before[1].NoiseRX
	})
	after := freeze()
	for i := range r.stats {
		sent, received := after[i].NoiseTX-before[i].NoiseTX, after[1-i].NoiseRX-before[1-i].NoiseRX
		if sent == 0 || sent != received {
			t.Fatalf("noise byte accounting differs: direction=%d sent=%d received=%d", i, sent, received)
		}
		frames := recoveryNoiseSent(r.sessions[i]) - counts[i]
		if frames == 0 || sent < frames*uint64(r.value.NoiseMinBytes) || sent > frames*uint64(r.value.NoiseMaxBytes) {
			t.Fatalf("live noise bounds not respected: direction=%d frames=%d bytes=%d range=[%d,%d]",
				i, frames, sent, r.value.NoiseMinBytes, r.value.NoiseMaxBytes)
		}
		if after[i].BusinessTX != before[i].BusinessTX || after[i].BusinessRX != before[i].BusinessRX {
			t.Fatal("idle noise contaminated business counters", before[i], after[i])
		}
	}
}

func (r *noiseScenarioRun) waitEmpty(t *testing.T) {
	t.Helper()
	waitFor(t, "all business sockets and caches released", func() bool {
		for _, s := range r.sessions {
			s.mu.Lock()
			clean := len(s.streams) == 0 && s.pendingBytes == 0 && s.incomingBytes == 0
			s.mu.Unlock()
			if !clean {
				return false
			}
		}
		return r.f.backendCount.active.Load() == 0 && r.f.localCount.active.Load() == 0
	})
	r.waitScene(t, noiseEmpty)
}
