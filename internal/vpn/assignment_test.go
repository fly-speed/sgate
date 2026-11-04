package vpn

import (
	"context"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"
)

func TestAutomaticAddressesAcrossTransports(t *testing.T) {
	for _, wire := range []string{"quic", "tcp", "wss", "udp"} {
		t.Run(wire, func(t *testing.T) {
			server, cfg, remote := fixtureWire(t, 4, NoiseConfig{}, wire)
			cfg.ClientIP = netip.Addr{}
			for range 2 {
				result := Probe(context.Background(), cfg)
				if !result.OK || result.AddressInUse || result.ClientIP != "10.77.0.2" {
					t.Fatalf("automatic probe: %+v", result)
				}
			}
			server.mu.Lock()
			reserved := len(server.peers)
			server.mu.Unlock()
			if reserved != 0 {
				t.Fatal("probe reserved an address")
			}
			assigned := make(chan netip.Addr, 4)
			var installed atomic.Int32
			cfg.Assigned = func(ip netip.Addr) { assigned <- ip }
			cfg.Connected = func() (func(), error) {
				if !installed.CompareAndSwap(0, 1) {
					t.Error("old network was not cleaned before reassignment")
				}
				return func() { installed.Store(0) }, nil
			}
			local, cancel, _ := startClient(t, cfg)
			ip := waitAssignment(t, assigned)
			await(t, "first automatic client", func() bool { return cfg.Stats.Snapshot().Active == 1 })
			second := cfg
			second.Stats = &Stats{}
			second.Connected = nil
			other, _, _ := startClient(t, second)
			otherIP := waitAssignment(t, assigned)
			await(t, "second automatic client", func() bool { return second.Stats.Snapshot().Active == 1 })
			if ip == otherIP || !validClientIP(cfg.Network, ip) || !validClientIP(cfg.Network, otherIP) {
				t.Fatal("duplicate or invalid assignment", ip, otherIP)
			}
			for _, client := range []struct {
				dev *memoryDevice
				ip  netip.Addr
			}{{local, ip}, {other, otherIP}} {
				out := ipPacket(client.ip.String(), "8.8.8.8", 1280)
				client.dev.out <- out
				receivePacket(t, remote, out)
				in := ipPacket("8.8.8.8", client.ip.String(), 1280)
				remote.out <- in
				receivePacket(t, client.dev, in)
			}
			server.mu.Lock()
			old := server.peers[ip]
			server.mu.Unlock()
			old.conn.CloseWithError(0, "test automatic reconnect")
			newIP := waitAssignment(t, assigned)
			await(t, "automatic reconnect", func() bool { return cfg.Stats.Snapshot().Active == 1 && cfg.Stats.Snapshot().Connections >= 2 })
			packet := ipPacket(newIP.String(), "8.8.4.4", 1000)
			local.out <- packet
			receivePacket(t, remote, packet)
			cancel()
			await(t, "automatic lease release", func() bool {
				server.mu.Lock()
				defer server.mu.Unlock()
				return server.peers[newIP] == nil && installed.Load() == 0
			})
		})
	}
}

func waitAssignment(t *testing.T, assigned <-chan netip.Addr) netip.Addr {
	t.Helper()
	select {
	case ip := <-assigned:
		return ip
	case <-time.After(5 * time.Second):
		t.Fatal("no server assignment")
		return netip.Addr{}
	}
}

func TestAddressPoolExhaustionAndIdentityReuse(t *testing.T) {
	s := &Server{cfg: Config{Network: netip.MustParsePrefix("10.77.0.0/30")}, peers: map[netip.Addr]*peer{}}
	ip := s.allocateIPLocked("first")
	if ip.String() != "10.77.0.2" {
		t.Fatal(ip)
	}
	s.peers[ip] = &peer{id: "first", ip: ip}
	if s.allocateIPLocked("second").IsValid() {
		t.Fatal("exhausted pool assigned a reserved address")
	}
	if s.allocateIPLocked("first") != ip {
		t.Fatal("reconnecting identity lost its address")
	}
	delete(s.peers, ip)
	if s.allocateIPLocked("second") != ip {
		t.Fatal("released address was not reused")
	}
}

func TestIntegratedClientIgnoresLegacyFixedAddress(t *testing.T) {
	fs := integratedSettings(t)
	fs.Set("vpn-mode", "both")
	fs.Set("vpn-client-ip", "10.77.0.99")
	p, err := Prepare("client", fs)
	if err != nil {
		t.Fatal(err)
	}
	if p.Config.ClientIP.IsValid() || p.Network.ClientIP.IsValid() {
		t.Fatal("legacy fixed address still applied")
	}
	for _, setting := range Settings("client") {
		if setting.Key == "vpn-client-ip" {
			t.Fatal("fixed address offered in settings")
		}
	}
}
