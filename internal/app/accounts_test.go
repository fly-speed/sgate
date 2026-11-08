package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"sgate/internal/admin"
	"sgate/internal/tunnel"
)

func TestAccountsFileRelativePathAndApplyRollback(t *testing.T) {
	backend, _ := probeBackend(t)
	c := newController(t, "server", "target = "+backend+"\n")
	file := filepath.Join(filepath.Dir(c.path), "accounts.json")
	data := []byte(`{"version":1,"accounts":[{"username":"alice","password":"alice-secret"}]}`)
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	setConfig(t, c, "accounts-file", "accounts.json")
	c.args = append(c.args, "-password=")
	if err := c.apply(); err != nil {
		t.Fatal(err)
	}
	if len(c.current.server.Accounts) != 1 || c.current.server.Password != "" {
		t.Fatal("account-only mode requires legacy password")
	}
	cfg := tunnel.ClientConfig{Key: c.current.server.Key, Username: "alice", Password: "alice-secret", URL: "ws://" + c.worker.listener.Addr().String() + "/tunnel"}
	client, err := tunnel.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if result := client.Probe(context.Background()); !result.OK {
		t.Fatal("configured account rejected", result)
	}
	worker := c.worker
	if err := os.WriteFile(file, []byte(`{"version":1,"accounts":null}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.apply(); err == nil || c.worker != worker {
		t.Fatal("invalid account JSON replaced running proxy")
	}
	if result := client.Probe(context.Background()); !result.OK {
		t.Fatal("failed apply changed account policy", result)
	}
}

func TestAccountFlagsPreserveCLIAndClientMappingCredentials(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "accounts.json")
	if err := os.WriteFile(file, []byte(`{"version":1,"accounts":[{"username":"cli-user","password":"cli-secret"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	c := newController(t, "server", "target = localhost:80\n", "-accounts-file", file, "-password=")
	setConfig(t, c, "accounts-file", "missing-file.json")
	next, err := c.load("")
	if err != nil {
		t.Fatal(err)
	}
	if len(next.server.Accounts) != 1 || next.server.Accounts[0].Username != "cli-user" {
		t.Fatal("CLI account file priority lost")
	}
	client := newController(t, "client", testClientMapping(t, "ws://127.0.0.1:1/tunnel")+"username = file-user\n", "-username", "cli-user")
	setConfig(t, client, "username", "new-file-user")
	next, err = client.load("")
	if err != nil || next.client.Username != "cli-user" {
		t.Fatal("CLI username priority lost", err)
	}
	setConfig(t, client, "proxies", `[{"listen":"127.0.0.1:19001","server":"ws://localhost:8080/tunnel"},{"listen":"127.0.0.1:19002","server":"ws://localhost:8081/tunnel"}]`)
	next, err = client.load("")
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range next.proxies {
		if route.config.Username != "cli-user" {
			t.Fatal("mapping lost account credentials")
		}
	}
}

func TestClientSavedCredentialsApplyToServerConnection(t *testing.T) {
	backend, _ := probeBackend(t)
	server := newController(t, "server", "target = "+backend+"\n")
	file := filepath.Join(filepath.Dir(server.path), "accounts.json")
	if err := os.WriteFile(file, []byte(`{"version":1,"accounts":[{"username":"alice","password":"alice-secret"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	setConfig(t, server, "accounts-file", file)
	if err := server.apply(); err != nil {
		t.Fatal(err)
	}
	url := "ws://" + server.worker.listener.Addr().String() + "/tunnel"
	client := newController(t, "client", testClientMapping(t, url))
	setConfig(t, client, "username", "alice")
	setConfig(t, client, "password", "alice-secret")
	if err := client.apply(); err != nil {
		t.Fatal(err)
	}
	if client.current.client.Username != "alice" || client.current.client.Password != "alice-secret" {
		t.Fatal("saved credentials not applied to client")
	}
	probe, err := tunnel.NewClient(client.current.proxies[0].config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if result := probe.Probe(ctx); !result.OK {
		t.Fatal("saved credentials failed server authentication", result)
	}
}

func TestAccountEditsUseEffectiveFileAndKeepProxy(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "accounts.json")
	if err := os.WriteFile(file, []byte(`{"version":1,"accounts":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	backend, _ := probeBackend(t)
	c := newController(t, "server", "target = "+backend+"\n", "-accounts-file", file, "-password=")
	worker := c.worker
	snapshot, err := c.manageTunnelAccounts(nil)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Path != file || !snapshot.Enabled {
		t.Fatal("effective CLI-selected account file lost")
	}
	snapshot, err = c.manageTunnelAccounts(&admin.TunnelAccountChange{Revision: snapshot.Revision, Action: "create", Username: "alice", Password: "a-secret"})
	if err != nil {
		t.Fatal(err)
	}
	client, err := tunnel.NewClient(tunnel.ClientConfig{Key: c.current.server.Key, Username: "alice", Password: "a-secret", URL: "ws://" + worker.listener.Addr().String() + "/tunnel"})
	if err != nil {
		t.Fatal(err)
	}
	if result := client.Probe(context.Background()); !result.OK {
		t.Fatal("new account was not applied", result)
	}
	if c.worker != worker || c.generation != 1 {
		t.Fatal("account update restarted proxy")
	}
	if len(c.current.server.Accounts) != 1 {
		t.Fatal("runtime account snapshot not updated")
	}
}

func TestFirstAccountBootstrapsLegacyProxyWithoutRestart(t *testing.T) {
	backend, _ := probeBackend(t)
	c := newController(t, "server", "target = "+backend+"\naccounts-file = ''\n")
	worker := c.worker
	setConfig(t, c, "target", "localhost:99")
	snapshot, err := c.manageTunnelAccounts(nil)
	if err != nil || !snapshot.CanCreate {
		t.Fatal(err)
	}
	result, err := c.manageTunnelAccounts(&admin.TunnelAccountChange{Revision: snapshot.Revision, Action: "create", Username: "alice", Password: "a-secret"})
	if err != nil || !result.Enabled || !result.ConfigChanged {
		t.Fatal("first account failed", err)
	}
	if c.worker != worker || c.generation != 1 || c.current.server.Target != backend {
		t.Fatal("bootstrap restarted proxy or applied pending target")
	}
	client, err := tunnel.NewClient(tunnel.ClientConfig{Key: c.current.server.Key, Username: "alice", Password: "a-secret", URL: "ws://" + worker.listener.Addr().String() + "/tunnel"})
	if err != nil {
		t.Fatal(err)
	}
	if result := client.Probe(context.Background()); !result.OK {
		t.Fatal("first account not active", result)
	}
	next, err := c.load("")
	if err != nil || next.accountsFile != result.Path || len(next.server.Accounts) != 1 {
		t.Fatal("first account not persistent", err)
	}
}

func TestFirstAccountRespectsExplicitCLIEmptyPath(t *testing.T) {
	c := newController(t, "server", "target = localhost:80\n", "-accounts-file=")
	c.overrides = []string{"accounts-file"} // Same explicit override bookkeeping as NewServer.
	snapshot, err := c.manageTunnelAccounts(nil)
	if err != nil || snapshot.CanCreate || snapshot.SetupMessage == "" {
		t.Fatal("CLI blank path was ignored", err)
	}
	if _, err := c.manageTunnelAccounts(&admin.TunnelAccountChange{Action: "create", Username: "alice", Password: "secret", Revision: snapshot.Revision}); err == nil {
		t.Fatal("bootstrap overrode CLI")
	}
}
