package tunnel

import (
	"fmt"
	"net"
	"testing"
	"time"
)

// 保持128条真实业务TCP连接完全静默，跨越默认60s心跳超时窗口。
// 噪音只在服务端开启；必须验证对端收到，不能只观察本端定时器或网卡总量。
func TestNoiseIdleDefaultHeartbeat(t *testing.T) {
	for _, mode := range []string{"", "tls"} {
		t.Run("protocol="+mode, func(t *testing.T) {
			noiseRecoveryResourceCheck(t)
			serverStats, clientStats := NewStats(), NewStats()
			t.Cleanup(func() {
				for _, stats := range []*Stats{serverStats, clientStats} {
					stats.mu.Lock()
					sessions, transports := len(stats.sessions), len(stats.noiseSessions)
					stats.mu.Unlock()
					if sessions != 0 || transports != 0 {
						t.Errorf("retained observation references: sessions=%d transports=%d", sessions, transports)
					}
				}
			})
			opts := Options{Stats: serverStats, NoiseIntervalMin: 10 * time.Millisecond, NoiseIntervalMax: 100 * time.Millisecond,
				NoiseMinBytes: 10 * 1024, NoiseMaxBytes: 100 * 1024, NoiseSuppressTraffic: true, NoiseIdleAfter: time.Second,
				Logf: func(string, ...any) {}}
			f := newRecoveryFixtureOptions(t, 2*time.Minute, 2*time.Minute, mode, opts, func(cfg *ClientConfig) {
				cfg.Options = Options{Stats: clientStats, Logf: opts.Logf}
			})
			conns := make([]net.Conn, 128)
			for i := range conns {
				conns[i] = f.dial()
				defer conns[i].Close()
				if err := exchange(conns[i], []byte(fmt.Sprintf("before-idle-%03d", i))); err != nil {
					t.Fatal(err)
				}
			}
			waitFor(t, "all streams idle and noise received", func() bool {
				state := serverStats.Snapshot().NoiseActivity
				return state.Idle == 1 && state.Streams == 128 && state.Permitted == 1 && clientStats.Snapshot().NoiseRX > 0
			})
			original := f.remoteSession()
			before := serverStats.Snapshot()
			for second := 0; second < 65; second++ {
				time.Sleep(time.Second)
				now, received := serverStats.Snapshot(), clientStats.Snapshot()
				if now.NoiseActivity.Tunnels != 1 || now.NoiseActivity.Idle != 1 || now.NoiseActivity.Streams != 128 || now.NoiseActivity.Disconnects != 0 {
					t.Fatalf("idle tunnel disappeared at %ds: %+v", second+1, now)
				}
				if now.NoiseTX <= before.NoiseTX || received.NoiseRX == 0 || now.BusinessTX != before.BusinessTX || now.BusinessRX != before.BusinessRX {
					t.Fatalf("idle noise stalled or leaked into business at %ds: before=%+v now=%+v", second+1, before, now)
				}
				if second%15 == 0 || second == 64 {
					t.Logf("idle %ds: noise TX=%d RX=%d; state=%+v", second+1, now.NoiseTX, received.NoiseRX, now.NoiseActivity)
				}
				before = now
			}
			f.disconnect()
			waitFor(t, "retained business distinguished from online transport", func() bool {
				v := serverStats.Snapshot().NoiseActivity
				return v.Tunnels == 0 && v.Offline == 1 && v.OfflineStreams == 128 && v.Disconnects == 1 && v.LastDisconnect != ""
			})
			before = serverStats.Snapshot()
			time.Sleep(250 * time.Millisecond)
			if serverStats.Snapshot().NoiseTX != before.NoiseTX {
				t.Fatal("reported noise on disconnected transport")
			}
			f.blocked.Store(false)
			waitFor(t, "idle noise resumes on original business session", func() bool {
				v := serverStats.Snapshot()
				return v.NoiseActivity.Tunnels == 1 && v.NoiseActivity.Offline == 0 && v.NoiseTX > before.NoiseTX && f.remoteSession() == original
			})
			for i, c := range conns {
				// 夹具初始10s截止时间不能沿用到65s静默之后。
				_ = c.SetDeadline(time.Now().Add(10 * time.Second))
				if err := exchange(c, []byte(fmt.Sprintf("after-idle-%03d", i))); err != nil {
					t.Fatal(err)
				}
				c.Close()
			}
			waitFor(t, "business sockets and replay released", func() bool {
				for _, s := range []*session{f.clientSession(), f.remoteSession()} {
					s.mu.Lock()
					clean := len(s.streams) == 0 && s.pendingBytes == 0 && s.incomingBytes == 0
					s.mu.Unlock()
					if !clean {
						return false
					}
				}
				return f.backendCount.active.Load() == 0 && f.localCount.active.Load() == 0
			})
		})
	}
}

// 断线保留与过期必须同时释放诊断引用，不能为了展示离线状态留下会话或缓存。
func TestNoiseOfflineObservationExpires(t *testing.T) {
	for _, mode := range []string{"", "tls"} {
		t.Run("protocol="+mode, func(t *testing.T) {
			noiseRecoveryResourceCheck(t)
			stats := NewStats()
			opts := testOptions()
			opts.Stats, opts.NoiseInterval = stats, 20*time.Millisecond
			f := newRecoveryFixtureOptions(t, 500*time.Millisecond, 500*time.Millisecond, mode, opts, func(c *ClientConfig) { c.Options.Stats = nil })
			c := f.dial()
			defer c.Close()
			if err := exchange(c, []byte("before-expiry")); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "noise before disconnect", func() bool { return stats.Snapshot().NoiseTX > 0 })
			f.disconnect()
			waitFor(t, "offline business visible", func() bool { return stats.Snapshot().NoiseActivity.OfflineStreams == 1 })
			waitFor(t, "expired session and cache released", func() bool {
				v := stats.Snapshot().NoiseActivity
				return v.Sessions == 0 && v.Streams == 0 && v.Tunnels == 0 && f.backendCount.active.Load() == 0
			})
			stats.mu.Lock()
			retained := len(stats.sessions) + len(stats.noiseSessions)
			stats.mu.Unlock()
			if retained != 0 {
				t.Fatal("expired diagnostic references", retained)
			}
			f.blocked.Store(false)
			waitFor(t, "new transport after expiry", func() bool { return stats.Snapshot().NoiseActivity.Tunnels == 1 })
			fresh := f.dial()
			defer fresh.Close()
			if err := exchange(fresh, []byte("new-session-after-expiry")); err != nil {
				t.Fatal(err)
			}
		})
	}
}
