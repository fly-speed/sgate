package tunnel

import (
	"testing"
	"time"
)

// An hour-long pending timer must be canceled, rather than merely happen to
// finish before the cleanup assertion. Cover each one-sided disable mode too.
func TestNoiseDirectionTimersReleaseOnExpiry(t *testing.T) {
	for _, tlsMode := range []string{"", "tls"} {
		for _, disabled := range []string{"client", "server", "both"} {
			t.Run(tlsMode+"/disable-"+disabled, func(t *testing.T) {
				noiseRecoveryResourceCheck(t)
				serverOpts, clientOpts := testOptions(), testOptions()
				if disabled != "server" && disabled != "both" {
					serverOpts.NoiseIntervalMin, serverOpts.NoiseIntervalMax = time.Hour, 2*time.Hour
				}
				if disabled != "client" && disabled != "both" {
					clientOpts.NoiseIntervalMin, clientOpts.NoiseIntervalMax = time.Hour, 2*time.Hour
				}
				f := newRecoveryFixtureOptions(t, 200*time.Millisecond, 250*time.Millisecond, tlsMode, serverOpts, func(c *ClientConfig) { c.Options = clientOpts })
				app := f.dial()
				if e := exchange(app, []byte("timer-cleanup")); e != nil {
					t.Fatal(e)
				}
				client, remote := f.clientSession(), f.remoteSession()
				f.disconnect()
				assertRecoveryNoiseStopped(t, client)
				assertRecoveryNoiseStopped(t, remote)
				f.fillQueues(app, client, remote)
				assertReleased(t, "expired client queue and timer", client)
				assertReleased(t, "expired server queue and timer", remote)
				assertSocketClosed(t, app)
				f.blocked.Store(false)
				f.assertFreshConnection()
				freshClient, freshRemote := f.clientSession(), f.remoteSession()
				f.cancel()
				f.stopServer()
				for _, s := range []*session{client, remote, freshClient, freshRemote} {
					assertReleased(t, "shutdown queues and timer", s)
					if recoveryNoiseSent(s) != 0 {
						t.Fatal("disabled or hour-long pending noise timer fired")
					}
				}
				waitFor(t, "all application sockets released", func() bool { return f.backendCount.active.Load() == 0 && f.localCount.active.Load() == 0 })
			})
		}
	}
}
