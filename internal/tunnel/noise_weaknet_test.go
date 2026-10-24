package tunnel

import (
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func recoveryNoiseReceived(s *session) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.noiseReceived
}

// Called before impairment; the returned check proves padding was delivered
// during the impaired part of the test, rather than only during warmup.
func weakNoiseProgress(t *testing.T, f *recoveryFixture) func() {
	t.Helper()
	if !recoveryNoiseEnabled(f.cfg.Options) {
		return func() {}
	}
	client, remote := f.clientSession(), f.remoteSession()
	a, b := recoveryNoiseReceived(client), recoveryNoiseReceived(remote)
	return func() {
		t.Helper()
		if recoveryNoiseReceived(client) <= a || recoveryNoiseReceived(remote) <= b {
			t.Fatal("padding was not delivered in both directions under weak network")
		}
	}
}

func TestNoiseWeakNetwork(t *testing.T) {
	noiseRecoveryResourceCheck(t)
	cases := []struct {
		name string
		run  func(*testing.T, func(*Options), string)
	}{
		{"slow-frame", testWeakNetworkSlowFrameProgressDoesNotReconnect},
		{"latency-jitter-retransmission", testWeakNetworkLatencyJitterAndRetransmission},
		{"short-one-way-stall", testWeakNetworkShortDirectionalBlackholes},
		{"one-way-resume", testWeakNetworkDirectionalOutagesResumeWithoutDuplicates},
		{"one-way-expiry", testWeakNetworkDirectionalExpiryReleasesBothSides},
	}
	for _, mode := range []string{"", "tls"} {
		protocol := "http"
		if mode == "tls" {
			protocol = "https"
		}
		for _, c := range cases {
			t.Run(protocol+"/"+c.name, func(t *testing.T) {
				noiseRecoveryResourceCheck(t)
				c.run(t, func(o *Options) {
					o.NoiseIntervalMin, o.NoiseIntervalMax = 100*time.Millisecond, 200*time.Millisecond
					o.NoiseMinBytes, o.NoiseMaxBytes = 1024, 10240
					if c.name == "slow-frame" {
						// Keep offered padding below this deliberately throttled
						// relay's capacity. TLS must receive a full record before
						// it can report progress to the WebSocket reader.
						o.NoiseIntervalMin, o.NoiseIntervalMax = 300*time.Millisecond, 600*time.Millisecond
						if mode == "tls" {
							o.HeartbeatTimeout = time.Second
						}
					}
				}, mode)
			})
		}
	}
}

// The two senders generate different byte streams without waiting for each
// other. Sequence records, a terminal marker and EOF detect lost/repeated tails.
func TestNoiseWeakNetworkIndependentDuplex(t *testing.T) {
	noiseRecoveryResourceCheck(t)
	for _, mode := range []string{"", "tls"} {
		for _, direction := range []int{-1, 0, 1} {
			name := "healthy"
			if direction >= 0 {
				name = []string{"uplink-resume", "downlink-resume"}[direction]
			}
			protocol := "http"
			if mode == "tls" {
				protocol = "https"
			}
			t.Run(protocol+"/"+name, func(t *testing.T) {
				noiseRecoveryResourceCheck(t)
				opts := Options{Heartbeat: 100 * time.Millisecond, HeartbeatTimeout: 2 * time.Second,
					DialTimeout: time.Second, WriteTimeout: time.Second, BufferBytes: 128 * 1024,
					NoiseIntervalMin: 200 * time.Millisecond, NoiseIntervalMax: 400 * time.Millisecond,
					NoiseMinBytes: 1024, NoiseMaxBytes: 10240, Logf: func(string, ...any) {}}
				if direction >= 0 {
					opts.HeartbeatTimeout = 600 * time.Millisecond
					opts.DialTimeout, opts.WriteTimeout = 300*time.Millisecond, 300*time.Millisecond
				}
				var upSent, upReceived, downSent, downReceived atomic.Int64
				backendDone := make(chan error, 1)
				end := time.Now().Add(3 * time.Second)
				f, relay := weakFixtureBackend(t, opts, 8*time.Second, 8*time.Second, mode, func(c net.Conn) {
					_ = c.SetDeadline(time.Now().Add(10 * time.Second))
					backendDone <- stressDuplex(c, 42, end, 4096, true, &downSent, &upReceived, 100*time.Millisecond)
				})
				warmupRecoveryNoise(t, f)
				client, remote := f.clientSession(), f.remoteSession()
				checkNoise := weakNoiseProgress(t, f)
				accepted := relay.accepted.Load()
				relay.profile.Store(&weakProfile{
					delay:       150 * time.Millisecond,
					jitter:      100 * time.Millisecond,
					lossPercent: 15,
					retransmit:  200 * time.Millisecond,
				})
				if direction >= 0 {
					relay.profile.Store(&weakProfile{
						delay:       20 * time.Millisecond,
						jitter:      10 * time.Millisecond,
						lossPercent: 10,
						retransmit:  50 * time.Millisecond,
					})
				}
				app := f.dial()
				defer app.Close()
				clientDone := make(chan error, 1)
				go func() {
					clientDone <- stressDuplex(app, 42, end, 4096, false, &upSent, &downReceived, 100*time.Millisecond)
				}()
				if direction >= 0 {
					waitFor(
						t,
						"both independent streams started",
						func() bool { return upReceived.Load() > 0 && downReceived.Load() > 0 },
					)
					relay.blackhole[direction].Store(true)
					waitFor(t, "noisy duplex heartbeat reconnect", func() bool { return relay.accepted.Load() > accepted })
					relay.blackhole[direction].Store(false)
				}
				for _, done := range []chan error{clientDone, backendDone} {
					select {
					case err := <-done:
						if err != nil {
							t.Fatal(err)
						}
					case <-time.After(10 * time.Second):
						t.Fatal("independent duplex stalled")
					}
				}
				if upSent.Load() == 0 || downSent.Load() == 0 || upSent.Load() != upReceived.Load() ||
					downSent.Load() != downReceived.Load() {
					t.Fatalf(
						"duplex totals: up %d/%d down %d/%d",
						upSent.Load(),
						upReceived.Load(),
						downSent.Load(),
						downReceived.Load(),
					)
				}
				if f.clientSession() != client || f.remoteSession() != remote || f.backendCount.accepted.Load() != 1 {
					t.Fatal("weak network replaced original session or application connection")
				}
				if direction < 0 && relay.accepted.Load() != accepted {
					t.Fatal("healthy noisy duplex reconnected")
				}
				if relay.losses.Load() == 0 {
					t.Fatal("no synthetic retransmission injected")
				}
				checkNoise()
				waitFor(t, "duplex sockets and replay buffers released", func() bool {
					if f.backendCount.active.Load() != 0 || f.localCount.active.Load() != 0 {
						return false
					}
					for _, s := range []*session{client, remote} {
						s.mu.Lock()
						clean := len(s.streams) == 0 && len(s.pending) == 0 && s.pendingBytes == 0
						s.mu.Unlock()
						if !clean {
							return false
						}
					}
					return true
				})
				t.Logf(
					"independent duplex verified: up=%d down=%d bytes; retransmissions=%d reconnects=%d",
					upSent.Load(),
					downSent.Load(),
					relay.losses.Load(),
					relay.accepted.Load()-accepted,
				)
			})
		}
	}
}

func TestNoiseWeakNetworkRealIntervals(t *testing.T) {
	for _, mode := range []string{"", "tls"} {
		protocol := "http"
		if mode == "tls" {
			protocol = "https"
		}
		t.Run(protocol, func(t *testing.T) {
			noiseRecoveryResourceCheck(t)
			testWeakNetworkLatencyJitterAndRetransmission(t, func(o *Options) {
				o.NoiseIntervalMin, o.NoiseIntervalMax = time.Second, 3*time.Second
				o.NoiseMinBytes, o.NoiseMaxBytes = 1024, 10240
			}, mode)
		})
	}
}

func TestNoiseWeakNetworkBusyWriter(t *testing.T) {
	noiseRecoveryResourceCheck(t)
	testHeartbeatNotStarvedByBusyWriter(t, func(o *Options) {
		o.NoiseIntervalMin, o.NoiseIntervalMax = time.Millisecond, 3*time.Millisecond
		o.NoiseMinBytes, o.NoiseMaxBytes = 1024, 10240
	})
}
