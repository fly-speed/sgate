package vpn

import (
	"flag"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestVPNClientToClientPolicy(t *testing.T) {
	for _, wire := range []string{"quic", "tcp", "wss", "udp", "mixed"} {
		t.Run(wire, func(t *testing.T) {
			server, cfg, remote := fixtureWire(t, 3, NoiseConfig{}, wire)
			local, _, _ := startClient(t, cfg)
			second := cfg
			second.ClientIP = netip.MustParseAddr("10.77.0.3")
			second.Stats = &Stats{}
			if wire == "mixed" {
				second.Transport = "wss"
				second.Address = server.TCPAddr().String()
			}
			other, _, _ := startClient(t, second)
			await(t, "both clients connected", func() bool { return len(server.Clients()) == 2 })
			out := ipPacket(cfg.ClientIP.String(), second.ClientIP.String(), 100)
			in := ipPacket(second.ClientIP.String(), cfg.ClientIP.String(), 120)
			policy := server.cfg.Live.Snapshot()
			if policy.AllowClientToClient {
				t.Fatal("client communication enabled by default")
			}
			assertDropped := func(send func()) {
				t.Helper()
				before := server.cfg.Stats.Snapshot().Drops
				send()
				await(t, "packet rejected", func() bool { return server.cfg.Stats.Snapshot().Drops > before })
				for _, dev := range []*memoryDevice{local, other, remote} {
					select {
					case <-dev.in:
						t.Fatal("blocked packet delivered")
					default:
					}
				}
			}
			assertDropped(func() { local.out <- out })
			policy.AllowPrivate = true
			if err := server.cfg.Live.Update(policy); err != nil {
				t.Fatal(err)
			}
			assertDropped(func() { local.out <- out })
			policy.AllowPrivate = false
			policy.AllowClientToClient = true
			if err := server.cfg.Live.Update(policy); err != nil {
				t.Fatal(err)
			}
			local.out <- out
			receivePacket(t, other, out)
			other.out <- in
			receivePacket(t, local, in)
			await(t, "client forwarding counters", func() bool {
				return server.cfg.Stats.Snapshot().RX == 220 && server.cfg.Stats.Snapshot().TX == 220
			})
			clients := server.Clients()
			for _, client := range clients {
				if client.VPNIP == cfg.ClientIP.String() && (client.Stats.RX != 100 || client.Stats.TX != 120) {
					t.Fatal(client)
				}
				if client.VPNIP == second.ClientIP.String() && (client.Stats.RX != 120 || client.Stats.TX != 100) {
					t.Fatal(client)
				}
			}
			// Neither unknown VPN destinations nor forged sources may reach the TUN.
			assertDropped(func() { local.out <- ipPacket(cfg.ClientIP.String(), "10.77.0.99", 60) })
			// The client itself rejects packets with a forged source before sending.
			forgedDrops := cfg.Stats.Snapshot().Drops
			local.out <- ipPacket("10.77.0.4", second.ClientIP.String(), 60)
			await(t, "forged source rejected", func() bool { return cfg.Stats.Snapshot().Drops > forgedDrops })
			assertDropped(func() { local.out <- ipPacket(cfg.ClientIP.String(), "192.168.1.2", 60) })
			gateway := ipPacket(cfg.ClientIP.String(), "10.77.0.1", 60)
			local.out <- gateway
			receivePacket(t, remote, gateway)
			policy.AllowClientToClient = false
			policy.AllowPrivate = true
			if err := server.cfg.Live.Update(policy); err != nil {
				t.Fatal(err)
			}
			assertDropped(func() { local.out <- out })
			assertDropped(func() { other.out <- in })
			// Enforce the latest policy again at send time, including packets already
			// queued before the switch was turned off (and traffic from the host TUN).
			assertDropped(func() { remote.out <- out })
			for _, stats := range []*Stats{cfg.Stats, second.Stats} {
				if stats.Snapshot().Connections != 1 || stats.Snapshot().Active != 1 {
					t.Fatal("policy update reconnected client")
				}
			}
		})
	}
}

func TestVPNClientToClientSettings(t *testing.T) {
	const key = "vpn-allow-client-to-client"
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	RegisterFlags(fs, "server")
	f := fs.Lookup(key)
	if f == nil || f.DefValue != "false" || !slices.Contains(LiveFields("server"), key) {
		t.Fatal("missing default or live setting")
	}
	clientFlags := flag.NewFlagSet("client", flag.ContinueOnError)
	RegisterFlags(clientFlags, "client")
	if clientFlags.Lookup(key) != nil || slices.Contains(LiveFields("client"), key) {
		t.Fatal("server setting exposed to clients")
	}
	values := map[string]string{key: "true"}
	live, err := WithLiveValues(LiveConfig{}, values, []string{key})
	if err != nil || !live.AllowClientToClient || live.AllowPrivate {
		t.Fatal(live, err)
	}
	var cfg Config
	cfg.setLive(live)
	if !LiveFromConfig(cfg).AllowClientToClient {
		t.Fatal("live setting lost")
	}
	if _, err := WithLiveValues(live, map[string]string{key: "invalid"}, []string{key}); err == nil {
		t.Fatal("invalid boolean accepted")
	}
	reset, err := WithLiveValues(live, nil, []string{key})
	if err != nil || reset.AllowClientToClient {
		t.Fatal("missing value must restore default", err)
	}
}

func TestVPNClientToClientPreparePreservesFingerprint(t *testing.T) {
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	RegisterFlags(fs, "server")
	key := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(key, []byte(strings.Repeat("01", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	fs.String("key-file", key, "")
	fs.Set("vpn-mode", "only")
	fs.Set("vpn-quic-listen", "")
	fs.Set("vpn-udp-listen", "127.0.0.1:8445")
	before, err := Prepare("server", fs)
	if err != nil {
		t.Fatal(err)
	}
	if before.Config.AllowClientToClient {
		t.Fatal("enabled by default")
	}
	if err := fs.Set("vpn-allow-client-to-client", "true"); err != nil {
		t.Fatal(err)
	}
	after, err := Prepare("server", fs)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Config.AllowClientToClient || after.Config.AllowPrivate || before.Fingerprint != after.Fingerprint {
		t.Fatal("live policy changed static identity or private policy")
	}
}
