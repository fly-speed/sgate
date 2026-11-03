package vpn

import (
	"context"
	"fmt"
	"net/netip"
	"testing"
	"time"
)

func TestVPNLiveNoiseAndPolicyDoNotReconnect(t *testing.T) {
	for _, wire := range []string{"quic", "tcp", "wss"} {
		t.Run(wire, func(t *testing.T) {
			server, cfg, remote := fixtureWire(t, 3, NoiseConfig{}, wire)
			cfg.Live, _ = NewLiveSettings(LiveConfig{Noise: NoiseConfig{Idle: true}})
			local, _, _ := startClient(t, cfg)
			await(t, "connected", func() bool { return cfg.Stats.Snapshot().Active == 1 })
			for _, live := range []*LiveSettings{server.cfg.Live, cfg.Live} {
				v := live.Snapshot()
				v.Noise = NoiseConfig{Rate: 8192, Idle: true, IdleAfter: time.Millisecond}
				if err := live.Update(v); err != nil {
					t.Fatal(err)
				}
			}
			await(t, "online padding enabled from zero", func() bool { return cfg.Stats.Snapshot().NoiseTX > 0 && cfg.Stats.Snapshot().NoiseRX > 0 })
			for _, live := range []*LiveSettings{server.cfg.Live, cfg.Live} {
				v := live.Snapshot()
				v.Noise.Rate = 0
				live.Update(v)
			}
			time.Sleep(60 * time.Millisecond)
			before := cfg.Stats.Snapshot().NoiseTX
			time.Sleep(60 * time.Millisecond)
			if cfg.Stats.Snapshot().NoiseTX != before {
				t.Fatal("padding did not stop")
			}
			policy := server.cfg.Live.Snapshot()
			policy.AllowPrivate = true
			server.cfg.Live.Update(policy)
			out := ipPacket(cfg.ClientIP.String(), "10.1.2.3", 1280)
			local.out <- out
			receivePacket(t, remote, out)
			in := ipPacket("10.1.2.3", cfg.ClientIP.String(), 1280)
			remote.out <- in
			receivePacket(t, local, in)
			drops := server.cfg.Stats.Snapshot().Drops
			policy.AllowPrivate = false
			server.cfg.Live.Update(policy)
			local.out <- out
			remote.out <- in
			await(t, "private access revoked both directions", func() bool { return server.cfg.Stats.Snapshot().Drops >= drops+2 })
			select {
			case <-remote.in:
				t.Fatal("revoked private packet forwarded")
			default:
			}
			select {
			case <-local.in:
				t.Fatal("revoked private reply forwarded")
			default:
			}
			public := ipPacket(cfg.ClientIP.String(), "8.8.8.8", 1280)
			local.out <- public
			receivePacket(t, remote, public)
			policy.MaxClients = 1
			server.cfg.Live.Update(policy)
			probeCfg := cfg
			probeCfg.ClientIP = netip.MustParseAddr("10.77.0.3")
			result := Probe(context.Background(), probeCfg)
			if !result.OK || !result.ClientLimitReached {
				t.Fatalf("live client limit ignored: %+v", result)
			}
			policy.MaxClients = 2
			server.cfg.Live.Update(policy)
			result = Probe(context.Background(), probeCfg)
			if !result.OK || result.ClientLimitReached {
				t.Fatalf("raised limit ignored: %+v", result)
			}
			if cfg.Stats.Snapshot().Connections != 1 || cfg.Stats.Snapshot().Active != 1 {
				t.Fatal("live update replaced VPN connection")
			}
		})
	}
}
func TestVPNLiveQueueAgeAndRejectedUpdate(t *testing.T) {
	live, _ := NewLiveSettings(LiveConfig{})
	stats := &Stats{}
	q := newPacketQueue(0, live)
	now := time.Now()
	q.push(ipPacket("10.77.0.2", "8.8.8.8", 60), now, stats)
	v, changed := live.Watch()
	v.QueueMaxAge = time.Millisecond
	live.Update(v)
	select {
	case <-changed:
	default:
		t.Fatal("update notification lost")
	}
	if q.pop(now.Add(time.Second), stats) != nil || stats.Snapshot().QueueExpired != 1 {
		t.Fatal("queued packet did not use new age")
	}
	before := live.Snapshot()
	bad := before
	bad.Noise.Rate = -1
	if live.Update(bad) == nil || live.Snapshot() != before {
		t.Fatal("invalid update changed live configuration")
	}
	for i := 0; i < 100; i++ {
		v, notify := live.Watch()
		v.Noise.Rate = i + 1
		if err := live.Update(v); err != nil {
			t.Fatal(err)
		}
		select {
		case <-notify:
		default:
			t.Fatal(fmt.Sprint("lost notification ", i))
		}
	}
}

func TestLoweredVPNLimitKeepsExistingClientsAndDiagnosticCapacity(t *testing.T) {
	server, cfg, _ := fixtureWire(t, 20, NoiseConfig{}, "quic")
	for i := 0; i < 20; i++ {
		client := cfg
		client.ClientIP = netip.MustParseAddr(fmt.Sprintf("10.77.0.%d", i+2))
		client.Stats = &Stats{}
		startClient(t, client)
		await(t, "client admission", func() bool { return client.Stats.Snapshot().Active == 1 })
	}
	next := server.cfg.Live.Snapshot()
	next.MaxClients = 1
	server.cfg.Live.Update(next)
	result := Probe(context.Background(), cfg)
	if !result.OK || !result.ClientLimitReached || server.cfg.Stats.Snapshot().Active != 20 {
		t.Fatalf("lowered limit disrupted diagnostics or clients: %+v", result)
	}
}
