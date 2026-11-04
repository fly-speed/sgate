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
