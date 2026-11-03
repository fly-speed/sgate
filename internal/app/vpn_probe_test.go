package app

import (
	"context"
	"crypto/tls"
	"net/netip"
	"os"
	"testing"
	"time"

	"sgate/internal/cli"
	"sgate/internal/vpn"
)

func TestVPNProbeReadsDormantSettingsAndDraftsWithoutApplying(t *testing.T) {
	c := newController(t, "client", "server = ws://127.0.0.1:1/tunnel\n")
	cert, private := managedCertificate(t)
	pair, e := tls.LoadX509KeyPair(cert, private)
	if e != nil {
		t.Fatal(e)
	}
	key, e := cli.LoadKey(c.original.Lookup("key-file").Value.String())
	if e != nil {
		t.Fatal(e)
	}
	dev := &vpnTestDevice{done: make(chan struct{})}
	server, e := vpn.ListenServer(vpn.Config{Address: "127.0.0.1:0", Key: key, TLS: &tls.Config{Certificates: []tls.Certificate{pair}}, Network: netip.MustParsePrefix("10.77.0.0/24"), MTU: 1280}, dev)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		server.Close()
		select {
		case e := <-done:
			if e != nil {
				t.Error(e)
			}
		case <-time.After(5 * time.Second):
			t.Error("probe server did not stop")
		}
	})
	url := "quic://" + server.Addr().String() + "/vpn"
	setConfig(t, c, "vpn-mode", "disabled")
	setConfig(t, c, "vpn-server", url)
	setConfig(t, c, "vpn-tls-ca", cert)
	before, _ := os.ReadFile(c.path)
	worker := c.worker
	generation := c.generation
	result, e := c.testVPN(context.Background(), nil)
	if e != nil || !result.OK {
		t.Fatal(result, e)
	}
	if result.Target != url || c.worker != worker || c.generation != generation || c.current.vpn != nil {
		t.Fatal("probe applied dormant VPN settings")
	}
	result, e = c.testVPN(context.Background(), map[string]string{"vpn-server": "wss://invalid"})
	if e != nil || result.OK || result.Stage != "config" {
		t.Fatal(result, e)
	}
	c.overrides = []string{"vpn-server"}
	c.args = append(c.args, "-vpn-server="+url)
	result, e = c.testVPN(context.Background(), map[string]string{"vpn-server": "wss://invalid"})
	if e != nil || !result.OK {
		t.Fatal("CLI override lost", result, e)
	}
	after, _ := os.ReadFile(c.path)
	if string(before) != string(after) {
		t.Fatal("probe saved config")
	}
}
