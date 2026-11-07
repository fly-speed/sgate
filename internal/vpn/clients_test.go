package vpn

import (
	"context"
	"net"
	"net/netip"
	"testing"
)

func TestServerClientsReflectAuthenticatedAddressesAndDisconnect(t *testing.T) {
	for _, wire := range []string{"quic", "tcp", "wss", "udp"} {
		t.Run(wire, func(t *testing.T) {
			s, c, _ := fixtureWire(t, 4, NoiseConfig{}, wire)
			if len(s.Clients()) != 0 {
				t.Fatal("unexpected client")
			}
			result := Probe(context.Background(), c)
			if !result.OK {
				t.Fatal(result)
			}
			if len(s.Clients()) != 0 {
				t.Fatal("probe appeared as an online client")
			}
			c.ClientIP = netip.Addr{}
			_, cancel, _ := startClient(t, c)
			await(t, "authenticated client", func() bool { return c.Stats.Snapshot().Active == 1 })
			clients := s.Clients()
			if len(clients) != 1 {
				t.Fatal(clients)
			}
			client := clients[0]
			host, _, err := net.SplitHostPort(client.RemoteAddress)
			if err != nil || client.PublicIP != host || host != "127.0.0.1" || client.VPNIP != "10.77.0.2" || client.Transport != wire {
				t.Fatal(client, err)
			}
			cancel()
			await(t, "disconnected client disappears", func() bool { return len(s.Clients()) == 0 })
		})
	}
}

func TestServerClientTrafficIsolatedAcrossTransports(t *testing.T) {
	for _, wire := range []string{"quic", "tcp", "wss", "udp"} {
		t.Run(wire, func(t *testing.T) {
			noise := NoiseConfig{Rate: 2048, Traffic: true, Idle: true}
			s, c, remote := fixtureWire(t, 4, noise, wire)
			c.Noise = noise
			local, _, _ := startClient(t, c)
			second := c
			second.ClientIP = netip.MustParseAddr("10.77.0.3")
			second.Stats = &Stats{}
			other, _, _ := startClient(t, second)
			await(t, "both clients", func() bool { return len(s.Clients()) == 2 })
			for i, dev := range []*memoryDevice{local, other} {
				ip := []string{"10.77.0.2", "10.77.0.3"}[i]
				out := ipPacket(ip, "8.8.8.8", 60+i*40)
				dev.out <- out
				receivePacket(t, remote, out)
				in := ipPacket("8.8.8.8", ip, 80+i*40)
				remote.out <- in
				receivePacket(t, dev, in)
			}
			await(t, "separate business and padding counters", func() bool {
				clients := s.Clients()
				if len(clients) != 2 {
					return false
				}
				for i, client := range clients {
					if client.Generation == "" || client.Stats.TX != uint64(80+i*40) || client.Stats.RX != uint64(60+i*40) || client.Stats.NoiseTX == 0 || client.Stats.NoiseRX == 0 {
						return false
					}
				}
				return true
			})
			old := s.Clients()[0]
			s.mu.Lock()
			p := s.peers[c.ClientIP]
			s.mu.Unlock()
			p.conn.CloseWithError(0, "test reconnect counters")
			await(t, "new connection resets only its own counters", func() bool {
				clients := s.Clients()
				return len(clients) == 2 && clients[0].Generation != old.Generation && clients[0].Stats.TX == 0 && clients[0].Stats.RX == 0 && clients[1].Stats.TX == 120 && clients[1].Stats.RX == 100
			})
		})
	}
}
