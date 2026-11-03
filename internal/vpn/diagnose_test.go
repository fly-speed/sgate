package vpn

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"testing"
)

func TestVPNDiagnosticOwnsOnlyExplicitPublicHostRoutes(t *testing.T) {
	n := NetworkOptions{Network: netip.MustParsePrefix("10.77.0.0/24"), Endpoint: netip.MustParseAddr("104.194.73.54"), Routes: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}, DNS: []netip.Addr{netip.MustParseAddr("1.1.1.1")}, DNSService: "Wi-Fi", BlockIPv6: true}
	addresses := map[string]netip.Addr{}
	for _, h := range diagnosticHosts {
		addresses[h] = netip.MustParseAddr("8.8.8.8")
	}
	got, err := diagnosticNetwork(n, addresses)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Routes) != 1 || got.Routes[0].String() != "8.8.8.8/32" || len(got.DNS) != 0 || got.BlockIPv6 || got.DNSService != "" {
		t.Fatal("diagnostic changes default route or DNS", got)
	}
	if len(n.DNS) != 1 || n.Routes[0].Bits() != 0 || !n.BlockIPv6 {
		t.Fatal("input settings mutated")
	}
	for _, bad := range []string{"10.77.0.2", "127.0.0.1", "192.168.1.1", "104.194.73.54", "::1"} {
		addresses[diagnosticHosts[0]] = netip.MustParseAddr(bad)
		if _, err := diagnosticNetwork(n, addresses); err == nil {
			t.Fatal("unsafe destination accepted", bad)
		}
	}
}
func TestVPNDiagnosticRejectsAnExistingLeaseWithoutTUN(t *testing.T) {
	_, cfg, _ := fixture(t, 4)
	startClient(t, cfg)
	await(t, "online peer", func() bool { return cfg.Stats.Snapshot().Active == 1 })
	host, port, _ := net.SplitHostPort(cfg.Address)
	p := &Prepared{Role: "client", Config: cfg, Host: host, Port: port}
	report := Diagnose(context.Background(), p)
	if !report.Probe.OK || !strings.Contains(report.Error, "already in use") || report.Device != "" || len(report.Routes) != 0 {
		t.Fatal("diagnostic displaced an existing peer", report)
	}
	if cfg.Stats.Snapshot().Active != 1 {
		t.Fatal("diagnostic interrupted existing connection")
	}
}
