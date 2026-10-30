package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"sgate/internal/admin"
	"sgate/internal/cli"
	"sgate/internal/tunnel"
)

func accountInitController(t *testing.T, content string, extra ...string) *controller {
	t.Helper()
	dir := t.TempDir()
	key := filepath.Join(dir, "key")
	path := filepath.Join(dir, "server.cf")
	if err := os.WriteFile(key, []byte(strings.Repeat("39", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("key-file = "+key+"\npassword = legacy-test-secret\n"+content), 0600); err != nil {
		t.Fatal(err)
	}
	return &controller{role: "server", path: path, original: appFlags("server"), args: append([]string{"-config", path}, extra...), stats: tunnel.NewStats()}
}

func startupServer(t *testing.T, c *controller) (*Server, error) {
	t.Helper()
	fs := appFlags("server")
	if err := cli.Parse(fs, c.args); err != nil {
		return nil, err
	}
	return NewServer(context.Background(), fs, c.args, "")
}

func TestStartupCreatesEmptyAccountsAndWebCanAddFirstUser(t *testing.T) {
	backend, _ := probeBackend(t)
	source := accountInitController(t, "target = "+backend+"\naccounts-file = private/tunnel-accounts.json\n")
	path := filepath.Join(filepath.Dir(source.path), "private", "tunnel-accounts.json")
	if _, err := source.load(""); err == nil {
		t.Fatal("validation unexpectedly created accounts")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("read-only validation created storage")
	}
	server, err := startupServer(t, source)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	accounts, err := tunnel.LoadAccounts(path)
	if err != nil || accounts == nil || len(accounts) != 0 || server.initial.server.Accounts == nil {
		t.Fatal("empty account mode not initialized", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]os.FileMode{path: 0600, filepath.Dir(path): 0700} {
		st, err := os.Stat(name)
		if err != nil || st.Mode().Perm() != want {
			t.Fatal("private storage permissions", err)
		}
	}
	// Start the initialized definition and use the same management callbacks as Gin.
	c := server.controller
	c.current = server.initial
	c.worker, err = c.current.start(c.ctx, c.stats)
	if err != nil {
		t.Fatal(err)
	}
	c.generation = 1
	worker := c.worker
	api := liveAccountManagement(t, c)
	empty := apiObject(t, api.call("GET", "/api/tunnel-accounts", nil))
	if empty["enabled"] != true || empty["can_create"] != true || len(empty["accounts"].([]any)) != 0 {
		t.Fatal("empty file not editable through Web", empty)
	}
	endpoint := "ws://" + worker.listener.Addr().String() + "/tunnel"
	probe := func(username, password string) bool {
		client, err := tunnel.NewClient(tunnel.ClientConfig{Key: c.current.server.Key, URL: endpoint, Username: username, Password: password})
		if err != nil {
			t.Fatal(err)
		}
		return client.Probe(context.Background()).OK
	}
	if probe("", "legacy-test-secret") || probe("alice", "alice-secret") {
		t.Fatal("empty file accepted a tunnel")
	}
	w := api.call("PUT", "/api/tunnel-accounts", admin.TunnelAccountChange{Action: "create", Username: "alice", Password: "alice-secret", Revision: empty["revision"].(string)})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "alice-secret") {
		t.Fatal("password exposed")
	}
	if !probe("alice", "alice-secret") || probe("", "legacy-test-secret") {
		t.Fatal("first account not active or legacy fallback enabled")
	}
	if c.worker != worker || c.generation != 1 {
		t.Fatal("first account restarted proxy")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) == string(raw) {
		t.Fatal("first account not saved", err)
	}
	again, err := startupServer(t, source)
	if err != nil {
		t.Fatal(err)
	}
	again.Close()
	unchanged, _ := os.ReadFile(path)
	if string(unchanged) != string(after) {
		t.Fatal("repeat startup overwrote accounts")
	}
	if state := apiObject(t, api.call("GET", "/api/status", nil)); state["authenticated"] != true {
		t.Fatal("Web login session lost")
	}
}

func TestStartupAccountsCreationCLIAndExistingFiles(t *testing.T) {
	source := accountInitController(t, "target = localhost:80\naccounts-file = ignored.json\n", "-accounts-file=")
	server, err := startupServer(t, source)
	if err != nil {
		t.Fatal(err)
	}
	server.Close()
	ignored := filepath.Join(filepath.Dir(source.path), "ignored.json")
	if _, err := os.Stat(ignored); !os.IsNotExist(err) {
		t.Fatal("CLI disabled accounts still created file")
	}
	absolute := filepath.Join(t.TempDir(), "accounts.json")
	source.args = append(source.args, "-accounts-file", absolute, "-password=")
	server, err = startupServer(t, source)
	if err != nil {
		t.Fatal(err)
	}
	if server.initial.server.Accounts == nil || server.initial.server.Password != "" {
		t.Fatal("account-only startup failed")
	}
	server.Close()
	if _, err := os.Stat(ignored); !os.IsNotExist(err) {
		t.Fatal("overridden path created")
	}
	for _, data := range []string{"", "invalid-existing-data", `{"version":1,"accounts":null}`} {
		if err := os.WriteFile(absolute, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := startupServer(t, source); err == nil {
			t.Fatal("existing invalid document accepted")
		}
		unchanged, _ := os.ReadFile(absolute)
		if string(unchanged) != data {
			t.Fatal("existing invalid document overwritten")
		}
	}
}

func TestConcurrentAccountsStartupAndPermissionFailure(t *testing.T) {
	source := accountInitController(t, "target = localhost:80\naccounts-file = nested/accounts.json\n")
	var group sync.WaitGroup
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			server, err := startupServer(t, source)
			if err != nil {
				t.Error(err)
				return
			}
			server.Close()
		}()
	}
	group.Wait()
	path := filepath.Join(filepath.Dir(source.path), "nested", "accounts.json")
	if accounts, err := tunnel.LoadAccounts(path); err != nil || len(accounts) != 0 {
		t.Fatal("concurrent initialization corrupted file", err)
	}
	temps, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".sgate-config-*"))
	if len(temps) != 0 {
		t.Fatal("temporary files retained", temps)
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0700)
	protected := filepath.Join(dir, "accounts.json")
	source.args = append(source.args, "-accounts-file", protected)
	if _, err := startupServer(t, source); err == nil {
		t.Fatal("unwritable account storage reported success")
	}
	if _, err := os.Stat(protected); !os.IsNotExist(err) {
		t.Fatal("unwritable account storage left a file")
	}
}
