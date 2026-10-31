package tunnel

import (
	"testing"
	"time"
)

// 重现大带宽图中的“空白”：每250ms只有一个业务字节，仍会持续刷新1s空闲阈值。
func TestNoiseObservationTrickleAndRealIdle(t *testing.T) {
	for _, mode := range []string{"", "tls"} {
		t.Run("protocol="+mode, func(t *testing.T) {
			noiseRecoveryResourceCheck(t)
			serverStats, clientStats := NewStats(), NewStats()
			t.Cleanup(func() {
				// Server.Close cancels hijacked WebSocket handlers; the writer
				// unregisters asynchronously when transport unwinding finishes.
				waitFor(t, "observation transport cleanup", func() bool {
					for _, stats := range []*Stats{serverStats, clientStats} {
						stats.mu.Lock()
						remaining := len(stats.noiseSessions)
						stats.mu.Unlock()
						if remaining != 0 || stats.Snapshot().NoiseActivity.Tunnels != 0 {
							return false
						}
					}
					return true
				})
			})
			live, err := NewLiveSettings(LiveConfig{NoiseIntervalMin: 10 * time.Millisecond, NoiseIntervalMax: 100 * time.Millisecond,
				NoiseMinBytes: 10 * 1024, NoiseMaxBytes: 100 * 1024, NoiseSuppressTraffic: true, NoiseIdleAfter: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			opts := testOptions()
			opts.Live = live
			opts.Stats = serverStats
			f := newRecoveryFixtureOptions(t, 2*time.Minute, 2*time.Minute, mode, opts, func(cfg *ClientConfig) {
				cfg.Options.Live = nil
				cfg.Options.Stats = clientStats
			})
			c := f.dial()
			if err := exchange(c, []byte("x")); err != nil {
				t.Fatal(err)
			}
			time.Sleep(250 * time.Millisecond)
			before := serverStats.Snapshot()
			for i := 0; i < 8; i++ {
				if err := exchange(c, []byte("x")); err != nil {
					t.Fatal(err)
				}
				snapshot := serverStats.Snapshot()
				if snapshot.NoiseActivity.SuppressedTraffic != 1 || snapshot.NoiseActivity.Permitted != 0 {
					t.Fatal("small business traffic not reported as active", snapshot)
				}
				time.Sleep(250 * time.Millisecond)
			}
			trickle := serverStats.Snapshot()
			if trickle.BusinessTX-before.BusinessTX != 8 || trickle.BusinessRX-before.BusinessRX != 8 || trickle.NoiseTX != before.NoiseTX {
				t.Fatal("trickle reproduction differs", before, trickle)
			}
			t.Logf("trickle 2s: business TX/RX=8/8 bytes, noise delta=%d, state=%+v", trickle.NoiseTX-before.NoiseTX, trickle.NoiseActivity)
			waitFor(t, "idle noise actually sent and received", func() bool {
				v := serverStats.Snapshot()
				return v.NoiseActivity.Idle == 1 && v.NoiseActivity.Permitted == 1 &&
					v.NoiseTX >= trickle.NoiseTX+100*1024 && clientStats.Snapshot().NoiseRX >= trickle.NoiseTX+100*1024
			})
			idle := serverStats.Snapshot()
			if idle.BusinessTX != trickle.BusinessTX || idle.BusinessRX != trickle.BusinessRX {
				t.Fatal("noise mistaken for business data")
			}
			t.Logf("true idle: noise delta=%d bytes; state=%+v", idle.NoiseTX-trickle.NoiseTX, idle.NoiseActivity)
			value, _ := live.Snapshot()
			value.NoiseIdleAfter = 100 * time.Millisecond
			if err := live.Update(value); err != nil {
				t.Fatal(err)
			}
			before = serverStats.Snapshot()
			for i := 0; i < 8; i++ {
				if err := exchange(c, []byte("y")); err != nil {
					t.Fatal(err)
				}
				time.Sleep(250 * time.Millisecond)
			}
			after := serverStats.Snapshot()
			if after.NoiseTX < before.NoiseTX+100*1024 {
				t.Fatal("shorter threshold did not fill trickle gaps", before, after)
			}
			t.Logf("same trickle, 100ms threshold: noise delta=%d bytes", after.NoiseTX-before.NoiseTX)
			c.Close()
			waitFor(t, "last business socket and cache released", func() bool {
				for _, s := range []*session{f.clientSession(), f.remoteSession()} {
					s.mu.Lock()
					clean := len(s.streams) == 0 && s.pendingBytes == 0
					s.mu.Unlock()
					if !clean {
						return false
					}
				}
				return true
			})
			waitFor(t, "empty scene reported", func() bool { return serverStats.Snapshot().NoiseActivity.Empty == 1 })
			f.disconnect()
			waitFor(t, "old observation registrations released", func() bool {
				return serverStats.Snapshot().NoiseActivity.Tunnels == 0 && clientStats.Snapshot().NoiseActivity.Tunnels == 0
			})
			f.blocked.Store(false)
			waitFor(t, "reconnect registers exactly one transport per endpoint", func() bool {
				return serverStats.Snapshot().NoiseActivity.Tunnels == 1 && clientStats.Snapshot().NoiseActivity.Tunnels == 1
			})
			if snapshot := clientStats.Snapshot(); snapshot.RX < snapshot.NoiseRX {
				t.Fatal("wire stats excluded received noise", snapshot)
			}
		})
	}
}
