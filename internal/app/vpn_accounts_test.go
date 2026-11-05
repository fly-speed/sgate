package app

import (
	"context"
	"testing"
	"time"

	"sgate/internal/admin"
	"sgate/internal/vpn"
)

func TestVPNAccountChangesUpdateRunningServer(t *testing.T) {
	c := newController(t, "server", "listen = 127.0.0.1:0\ntarget = 127.0.0.1:9\n")
	setConfig(t, c, "vpn-mode", "both")
	setConfig(t, c, "vpn-listen", "")
	setConfig(t, c, "vpn-udp-listen", "127.0.0.1:0")
	setConfig(t, c, "vpn-manage-network", "false")
	prepared, err := c.prepareSavedVPN()
	if err != nil {
		t.Fatal(err)
	}
	c.current.vpn = prepared
	c.worker.startVPNWithDevice(c.ctx, prepared, func(string) (vpnDevice, error) {
		return &vpnTestDevice{done: make(chan struct{})}, nil
	})
	worker := c.worker.vpn
	deadline := time.Now().Add(3 * time.Second)
	h := worker.snapshot()
	for h.State == "starting" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
		h = worker.snapshot()
	}
	if h.State != "listening" {
		t.Fatal(h)
	}
	client := vpn.Config{Address: h.UDPAddress, Transport: "udp", Key: prepared.Config.Key, Network: prepared.Config.Network, MTU: prepared.Config.MTU}
	if result := vpn.Probe(c.ctx, client); !result.OK {
		t.Fatal(result)
	}
	ctx, cancel := context.WithCancel(c.ctx)
	defer cancel()
	client.Stats = &vpn.Stats{}
	done := make(chan error, 1)
	go func() { done <- vpn.RunClient(ctx, client, &vpnTestDevice{done: make(chan struct{})}) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("client did not stop")
		}
	}()
	deadline = time.Now().Add(3 * time.Second)
	for client.Stats.Snapshot().Active == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if client.Stats.Snapshot().Active != 1 {
		t.Fatal("existing client did not connect")
	}
	view, err := c.manageVPNAccounts(nil)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := c.manageVPNAccounts(&admin.VPNAccountChange{Revision: view.Revision, Action: "create", Username: "alice", Password: "test-secret"})
	if err != nil || !saved.LiveApplied || !saved.ConfigChanged {
		t.Fatal(saved.Accounts, saved.LiveApplied, err)
	}
	if c.worker.vpn != worker || worker.snapshot().Generation != h.Generation || worker.snapshot().Stats.Active != 1 {
		t.Fatal("account creation restarted VPN")
	}
	if result := vpn.Probe(c.ctx, client); result.OK {
		t.Fatal("missing credentials still accepted")
	}
	client.Username, client.Password = "alice", "test-secret"
	if result := vpn.Probe(c.ctx, client); !result.OK {
		t.Fatal(result)
	}
	if pending, _, err := c.vpnRestartPending(); pending || err != nil {
		t.Fatal("live account update left a pending restart", pending, err)
	}

	change := func(action, username, newUsername, password string, disabled *bool) {
		t.Helper()
		saved, err = c.manageVPNAccounts(&admin.VPNAccountChange{
			Revision: saved.Revision, Action: action, Username: username,
			NewUsername: newUsername, Password: password, Disabled: disabled,
		})
		if err != nil || !saved.LiveApplied {
			t.Fatal("account change was not applied live", action, err)
		}
	}
	waitActive := func(stats *vpn.Stats, want int64) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for stats.Snapshot().Active != want && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if stats.Snapshot().Active != want {
			t.Fatalf("active connections = %d, want %d", stats.Snapshot().Active, want)
		}
	}
	connect := func(username, password string) *vpn.Stats {
		t.Helper()
		cfg := client
		cfg.Username, cfg.Password, cfg.Stats = username, password, &vpn.Stats{}
		ctx, cancel := context.WithCancel(c.ctx)
		finished := make(chan error, 1)
		go func() { finished <- vpn.RunClient(ctx, cfg, &vpnTestDevice{done: make(chan struct{})}) }()
		t.Cleanup(func() {
			cancel()
			select {
			case err := <-finished:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(3 * time.Second):
				t.Error("client did not stop")
			}
		})
		waitActive(cfg.Stats, 1)
		return cfg.Stats
	}
	probe := func(username, password string, want bool) {
		t.Helper()
		cfg := client
		cfg.Username, cfg.Password = username, password
		if result := vpn.Probe(c.ctx, cfg); result.OK != want {
			t.Fatalf("probe for %s, want success %v: %+v", username, want, result)
		}
	}

	change("create", "bob", "", "bob-secret", nil)
	bob := connect("bob", "bob-secret")
	alice := connect("alice", "test-secret")
	change("update", "alice", "alice-new", "new-secret", nil)
	waitActive(alice, 0)
	probe("alice", "test-secret", false)
	probe("alice-new", "new-secret", true)
	alice = connect("alice-new", "new-secret")
	aliceSecond := connect("alice-new", "new-secret")
	disabled := true
	change("update", "alice-new", "", "", &disabled)
	waitActive(alice, 0)
	waitActive(aliceSecond, 0)
	probe("alice-new", "new-secret", false)
	disabled = false
	change("update", "alice-new", "", "", &disabled)
	waitActive(alice, 1)
	waitActive(aliceSecond, 1)
	change("delete", "alice-new", "", "", nil)
	waitActive(alice, 0)
	waitActive(aliceSecond, 0)
	probe("alice-new", "new-secret", false)
	probe("bob", "bob-secret", true)
	if b := bob.Snapshot(); b.Active != 1 || b.Connections != 1 {
		t.Fatal("unrelated account was disconnected", b)
	}
	if c.worker.vpn != worker || worker.snapshot().Generation != h.Generation {
		t.Fatal("account change restarted VPN")
	}
	if pending, _, err := c.vpnRestartPending(); pending || err != nil {
		t.Fatal("account change left a pending restart", pending, err)
	}
}
