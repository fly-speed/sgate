package app

import (
	"errors"
	"sgate/internal/vpn"
	"testing"
	"time"
)

func TestVPNServerSaveReconnectsOnlyVPNAndPreservesPendingMTU(t *testing.T) {
	c := newController(t, "client", "server = ws://127.0.0.1:1/tunnel\n")
	setConfig(t, c, "vpn-mode", "both")
	setConfig(t, c, "vpn-server", "tcp://127.0.0.1:8444/vpn")
	next, err := c.load("")
	if err != nil {
		t.Fatal(err)
	}
	c.current.vpn = next.vpn
	stopped := false
	done := make(chan struct{})
	live, _ := vpn.NewLiveSettings(vpn.LiveFromConfig(next.vpn.Config))
	old := &vpnWorker{live: live, stats: &vpn.Stats{}, done: done, cancel: func() { stopped = true; close(done) }}
	c.worker.vpn = old
	opened := make(chan struct{}, 1)
	c.worker.openTUN = func(string) (vpnDevice, error) {
		if !stopped {
			t.Error("new TUN opened before old cleanup completed")
		}
		opened <- struct{}{}
		return nil, errors.New("test device unavailable")
	}
	worker, generation := c.worker, c.generation
	setConfig(t, c, "vpn-mtu", "1200")
	setConfig(t, c, "vpn-server", "wss://127.0.0.1:8445/vpn")
	if err = c.applyLive([]string{"vpn-server"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-opened:
	case <-time.After(3 * time.Second):
		t.Fatal("new VPN not started")
	}
	if c.worker != worker || c.generation != generation || c.worker.vpn == old || c.current.vpn.Config.Transport != "wss" || c.current.vpn.Config.MTU != 1280 || c.current.vpn.Network.MTU != 1280 {
		t.Fatal("endpoint update applied pending MTU or restarted proxy")
	}
}
func TestVPNServerSaveWhileStoppedDoesNotStartVPN(t *testing.T) {
	c := newController(t, "client", "server = ws://127.0.0.1:1/tunnel\n")
	setConfig(t, c, "vpn-mode", "both")
	setConfig(t, c, "vpn-server", "tcp://127.0.0.1:8444/vpn")
	next, err := c.load("")
	if err != nil {
		t.Fatal(err)
	}
	c.current.vpn = next.vpn
	c.worker.openTUN = func(string) (vpnDevice, error) {
		t.Error("stopped VPN started unexpectedly")
		return nil, errors.New("unexpected")
	}
	setConfig(t, c, "vpn-server", "wss://127.0.0.1:8445/vpn")
	if err = c.applyLive([]string{"vpn-server"}); err != nil {
		t.Fatal(err)
	}
	if c.worker.vpn != nil || c.current.vpn.Config.Address != "127.0.0.1:8445" {
		t.Fatal("stopped endpoint not updated")
	}
	before := c.current.vpn
	setConfig(t, c, "vpn-server", "ws://127.0.0.1:8445/vpn")
	if c.applyLive([]string{"vpn-server"}) == nil || c.current.vpn != before {
		t.Fatal("invalid endpoint changed applied state")
	}
}
func TestScopedVPNRestartRejectsProxyModeTransitionBeforeStopping(t *testing.T) {
	c := newController(t, "client", "server = ws://127.0.0.1:1/tunnel\n")
	setConfig(t, c, "vpn-mode", "only")
	setConfig(t, c, "vpn-server", "tcp://127.0.0.1:8444/vpn")
	worker, generation := c.worker, c.generation
	if c.restartVPN() == nil || c.worker != worker || c.generation != generation {
		t.Fatal("VPN-only restart applied a proxy mode change")
	}
}

func TestScopedVPNRestartAppliesSavedVPNButNotProxySettings(t *testing.T) {
	c := newController(t, "client", "server = ws://127.0.0.1:1/tunnel\nheartbeat = 20s\n")
	setConfig(t, c, "vpn-mode", "both")
	setConfig(t, c, "vpn-server", "tcp://127.0.0.1:8444/vpn")
	definition, err := c.load("")
	if err != nil {
		t.Fatal(err)
	}
	c.current.vpn = definition.vpn
	done := make(chan struct{})
	cleaned := false
	live, _ := vpn.NewLiveSettings(vpn.LiveFromConfig(definition.vpn.Config))
	c.worker.vpn = &vpnWorker{live: live, stats: &vpn.Stats{}, done: done, cancel: func() { cleaned = true; close(done) }}
	opened := make(chan struct{}, 1)
	c.worker.openTUN = func(string) (vpnDevice, error) {
		if !cleaned {
			t.Error("restart preceded cleanup")
		}
		opened <- struct{}{}
		return nil, errors.New("test device unavailable")
	}
	worker, generation := c.worker, c.generation
	setConfig(t, c, "heartbeat", "30s")
	if pending, _, err := c.vpnRestartPending(); err != nil || pending {
		t.Fatalf("proxy-only edit should not require a VPN restart: %v, %v", pending, err)
	}
	setConfig(t, c, "vpn-mtu", "1200")
	if pending, _, err := c.vpnRestartPending(); err != nil || !pending {
		t.Fatalf("saved VPN MTU should require a VPN restart: %v, %v", pending, err)
	}
	if err = c.restartVPN(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-opened:
	case <-time.After(3 * time.Second):
		t.Fatal("VPN was not restarted")
	}
	select {
	case <-c.worker.vpn.done:
	case <-time.After(3 * time.Second):
		t.Fatal("VPN worker did not report the test device failure")
	}
	if pending, _, err := c.vpnRestartPending(); err != nil || !pending {
		t.Fatalf("failed configuration restart should allow retry: %v, %v", pending, err)
	}
	if c.current.vpn.Config.MTU != 1200 || c.current.client.Options.Heartbeat != 20*time.Second || c.worker != worker || c.generation != generation {
		t.Fatal("scoped restart applied unrelated proxy changes")
	}
}
