package app

import (
	"context"
	"fmt"
	"testing"
	"time"

	"sgate/internal/tunnel"
)

func TestStoppedWorkerCannotAcknowledgeOnlineUpdate(t *testing.T) {
	live, err := tunnel.NewLiveSettings(tunnel.LiveConfig{})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	close(done)
	c := &controller{ctx: context.Background(), worker: &instance{live: live, done: done}}
	if c.applyLive([]string{"noise-interval"}) == nil {
		t.Fatal("stopped worker acknowledged online update")
	}
}

func TestContinuousLiveUpdatesKeepWorkerAndCommandLinePriority(t *testing.T) {
	for _, role := range []string{"client", "server"} {
		t.Run(role, func(t *testing.T) {
			text := "target = 127.0.0.1:1\n"
			extra := []string{"-noise-min-bytes", "256", "-noise-traffic=false", "-noise-idle-after=80ms"}
			if role == "client" {
				text = "server = ws://127.0.0.1:1/tunnel\n"
				extra = append(extra, "-reconnect-min", "400ms")
			}
			c := newController(t, role, text, extra...)
			worker := c.worker
			if role == "server" {
				setConfig(t, c, "target", "127.0.0.1:2")
			}
			for index := 0; index < 20; index++ {
				setConfig(t, c, "noise-interval", fmt.Sprintf("%dms", index+1))
				setConfig(t, c, "noise-min-bytes", "1K")
				setConfig(t, c, "noise-max-bytes", "32K")
				setConfig(t, c, "noise-traffic", "true")
				setConfig(t, c, "noise-idle", fmt.Sprint(index%2 == 0))
				setConfig(t, c, "noise-empty", fmt.Sprint(index%2 != 0))
				setConfig(t, c, "noise-idle-after", "2s")
				if role == "client" {
					setConfig(t, c, "reconnect-min", "100ms")
					setConfig(t, c, "reconnect-max", fmt.Sprintf("%ds", index+1))
				}
				if err := c.applyLive(c.liveFields()); err != nil {
					t.Fatal(err)
				}
				value, _ := worker.live.Snapshot()
				if c.worker != worker || c.generation != 1 {
					t.Fatal("live save replaced worker")
				}
				if value.NoiseMinBytes != 256 || value.NoiseMaxBytes != 32768 || value.NoiseInterval != time.Duration(index+1)*time.Millisecond {
					t.Fatal("live runtime or CLI value differs", value)
				}
				if !value.NoiseSuppressTraffic || value.NoiseIdleAfter != 80*time.Millisecond ||
					value.NoiseSuppressIdle != (index%2 != 0) || value.NoiseSuppressEmpty != (index%2 == 0) {
					t.Fatal("scenario live update or CLI precedence lost", value)
				}
				loaded, err := c.load("")
				if err != nil {
					t.Fatal(err)
				}
				if role == "client" {
					if value.ReconnectMin != 400*time.Millisecond || loaded.client.ReconnectMin != 400*time.Millisecond || value.ReconnectMax != time.Duration(index+1)*time.Second {
						t.Fatal("reconnect CLI precedence lost", value)
					}
				} else if c.current.server.Target != "127.0.0.1:1" || loaded.server.Target != "127.0.0.1:2" {
					t.Fatal("pending backend change was applied or discarded")
				}
			}
		})
	}
}
