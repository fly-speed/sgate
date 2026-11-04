package app

import (
	"testing"
	"time"

	"sgate/internal/vpn"
)

func TestVPNLiveSavePreservesPendingStaticSettingsAndCLIOverrides(t *testing.T) {
	for _, role := range []string{"client", "server"} {
		t.Run(role, func(t *testing.T) {
			text := "target = 127.0.0.1:1\n"
			if role == "client" {
				text = "server = ws://127.0.0.1:1/tunnel\n"
			}
			c := newController(t, role, text, "-vpn-noise-rate=1K")
			c.current.vpn = &vpn.Prepared{Role: role, Mode: "both", Fingerprint: "running-static", Config: vpn.Config{Address: "old.example:8444", Transport: "tcp", MTU: 1280, Noise: vpn.NoiseConfig{Rate: 1024}, MaxClients: 64}}
			live, _ := vpn.NewLiveSettings(vpn.LiveFromConfig(c.current.vpn.Config))
			done := make(chan struct{})
			worker := &vpnWorker{live: live, stats: &vpn.Stats{}, done: done, cancel: func() { close(done) }}
			c.worker.vpn = worker
			instance := c.worker
			generation := c.generation
			setConfig(t, c, "vpn-mode", "disabled")
			setConfig(t, c, "vpn-mtu", "invalid-but-dormant")
			setConfig(t, c, "vpn-noise-rate", "32K")
			setConfig(t, c, "vpn-noise-idle", "true")
			setConfig(t, c, "vpn-noise-idle-after", "7s")
			setConfig(t, c, "vpn-queue-max-age", "900ms")
			setConfig(t, c, "vpn-udp-send-interval", "1ms")
			if err := c.applyLive(vpn.LiveFields(role)); err != nil {
				t.Fatal(err)
			}
			got := live.Snapshot()
			if got.Noise.Rate != 1024 || !got.Noise.Idle || got.Noise.IdleAfter != 7*time.Second || got.QueueMaxAge != 900*time.Millisecond || got.UDPSendInterval != time.Millisecond {
				t.Fatal(got)
			}
			if c.worker != instance || c.worker.vpn != worker || c.generation != generation || c.current.vpn.Config.MTU != 1280 || c.current.vpn.Config.Address != "old.example:8444" || c.current.vpn.Mode != "both" || c.current.vpn.Fingerprint != "running-static" {
				t.Fatal("live save applied pending static settings")
			}
			if c.current.vpn.Config.UDPSendInterval != time.Millisecond {
				t.Fatal("UDP pacing interval was lost from the prepared configuration")
			}
			before := live.Snapshot()
			setConfig(t, c, "vpn-queue-max-age", "-1s")
			if c.applyLive([]string{"vpn-queue-max-age"}) == nil || live.Snapshot() != before {
				t.Fatal("invalid live save changed runtime")
			}
			setConfig(t, c, "vpn-udp-send-interval", "11ms")
			if c.applyLive([]string{"vpn-udp-send-interval"}) == nil || live.Snapshot() != before {
				t.Fatal("invalid UDP pacing interval changed runtime")
			}
		})
	}
}
