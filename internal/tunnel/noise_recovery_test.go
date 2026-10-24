package tunnel

import (
	"os"
	"runtime"
	"testing"
	"time"
)

func recoveryNoiseEnabled(o Options) bool { return o.NoiseInterval > 0 || o.NoiseIntervalMin > 0 }

func recoveryNoiseSent(s *session) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.noiseSent
}

func warmupRecoveryNoise(t *testing.T, f *recoveryFixture) {
	t.Helper()
	o := f.cfg.Options
	if !recoveryNoiseEnabled(o) || o.NoiseInterval > 3*time.Second || o.NoiseIntervalMax > 3*time.Second {
		return // The long-pending-timer test intentionally never sends padding.
	}
	waitFor(t, "both peers actually received random padding", func() bool {
		remote := f.remoteSession()
		if remote == nil {
			return false
		}
		client := f.clientSession()
		client.mu.Lock()
		a := client.noiseReceived > 0
		client.mu.Unlock()
		remote.mu.Lock()
		b := remote.noiseReceived > 0
		remote.mu.Unlock()
		return a && b
	})
}

func assertRecoveryNoiseStopped(t *testing.T, s *session) {
	t.Helper()
	waitFor(t, "old padding writer returned and stopped its timer", func() bool {
		if !s.transportMu.TryLock() {
			return false
		}
		defer s.transportMu.Unlock()
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.active == nil
	})
}

func noiseRecoveryResourceCheck(t *testing.T) {
	t.Helper()
	g, fd := runtime.NumGoroutine(), descriptorCount()
	// Registered before the fixture: runs after its sockets and goroutines close.
	t.Cleanup(func() {
		waitFor(t, "noise recovery goroutines released", func() bool { return runtime.NumGoroutine() <= g+3 })
		after := descriptorCount()
		if fd >= 0 && after > fd+2 {
			t.Errorf("descriptors grew: %d -> %d", fd, after)
		}
		t.Logf(
			"cleanup: goroutines=%d (baseline=%d), descriptors=%d (baseline=%d)",
			runtime.NumGoroutine(),
			g,
			after,
			fd,
		)
	})
}

func TestNoiseRecoveryScenarios(t *testing.T) {
	opts := testOptions()
	opts.NoiseIntervalMin, opts.NoiseIntervalMax = 5*time.Millisecond, 15*time.Millisecond
	opts.NoiseMinBytes, opts.NoiseMaxBytes = 1024, 10240
	cases := []struct {
		name string
		run  func(*testing.T, Options, string)
	}{
		{"long-outage", testLongOutageReleasesStateBeforeNetworkReturns},
		{"server-restart", testServerRestartDropsOldQueuesAndAcceptsFreshConnections},
		{"session-expiry", testSessionExpiryWithFullBuffersReleasesOldConnections},
		{"five-restarts", testRepeatedServerRestartsReleaseOldSessions},
	}
	for _, mode := range []string{"", "tls"} {
		for _, c := range cases {
			t.Run(mode+"/"+c.name, func(t *testing.T) {
				noiseRecoveryResourceCheck(t)
				c.run(t, opts, mode)
			})
		}
	}
}

func TestNoisePendingTimerReleasedOnExpiryAndShutdown(t *testing.T) {
	for _, mode := range []string{"", "tls"} {
		t.Run(mode, func(t *testing.T) {
			noiseRecoveryResourceCheck(t)
			opts := testOptions()
			opts.NoiseIntervalMin, opts.NoiseIntervalMax = time.Hour, 2*time.Hour
			f := newRecoveryFixtureOptions(t, 200*time.Millisecond, 250*time.Millisecond, mode, opts)
			app := f.dial()
			if err := exchange(app, []byte("pending-timer")); err != nil {
				t.Fatal(err)
			}
			client, remote := f.clientSession(), f.remoteSession()
			f.disconnect()
			f.fillQueues(app, client, remote)
			assertReleased(t, "client expiry with long pending timer", client)
			assertReleased(t, "server expiry with long pending timer", remote)
			assertSocketClosed(t, app)
			f.blocked.Store(false)
			f.assertFreshConnection()
			freshClient, freshRemote := f.clientSession(), f.remoteSession()
			f.cancel()
			f.stopServer()
			assertReleased(t, "client shutdown with pending timer", freshClient)
			assertReleased(t, "server shutdown with pending timer", freshRemote)
			for _, s := range []*session{client, remote, freshClient, freshRemote} {
				if recoveryNoiseSent(s) != 0 {
					t.Fatal("hour-long timer fired early")
				}
			}
		})
	}
}

func TestNoiseLongRecoveryDefaultOfflineWindow(t *testing.T) {
	if os.Getenv("SGATE_RECOVERY_LONG") != "1" {
		t.Skip("set SGATE_RECOVERY_LONG=1 for the real two-minute noisy outage")
	}
	noiseRecoveryResourceCheck(t)
	opts := testOptions()
	opts.NoiseIntervalMin, opts.NoiseIntervalMax = time.Second, 3*time.Second
	opts.NoiseMinBytes, opts.NoiseMaxBytes = 1024, 10240
	testLongRecoveryDefaultOfflineWindow(t, opts, "tls")
}
