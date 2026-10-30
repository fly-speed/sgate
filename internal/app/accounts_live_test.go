package app

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"sgate/internal/admin"
)

func liveAccountManagement(t *testing.T, c *controller) *managementAPI {
	t.Helper()
	dir := t.TempDir()
	web, _ := filepath.Abs("../../web/admin")
	cfg := admin.Config{WebDir: web, Listen: "127.0.0.1:9081", UsersFile: filepath.Join(dir, "users.json"), SessionDir: filepath.Join(dir, "sessions"), SetupTokenFile: filepath.Join(dir, "token"), SessionTimeout: time.Hour, MaxSessions: 10}
	h, err := admin.New(cfg, c.managementView())
	if err != nil {
		t.Fatal(err)
	}
	api := &managementAPI{h: h}
	token, err := os.ReadFile(cfg.SetupTokenFile)
	if err != nil {
		t.Fatal(err)
	}
	w := api.call("POST", "/api/setup", map[string]string{"username": "admin", "password": "test-admin-secret", "setup_token": strings.TrimSpace(string(token))})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	api.cookie = w.Result().Cookies()[0]
	api.csrf = apiObject(t, w)["csrf"].(string)
	return api
}

func TestAccountsFileOnlineSaveWSWSSKeepsWorkerAndUnchangedUsers(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprint(secure), func(t *testing.T) {
			backend := onlineBackend(t, "A")
			text := "target = " + backend + "\naccounts-file = ''\nnoise-interval-min = 1ms\nnoise-interval-max = 3ms\n"
			scheme, ca := "ws", ""
			if secure {
				cert, key := managedCertificate(t)
				text += "tls-cert = " + cert + "\ntls-key = " + key + "\n"
				scheme = "wss"
				ca = "tls-ca = " + cert + "\n"
			}
			c := newController(t, "server", text)
			worker := c.worker
			if !slices.Contains(c.liveFields(), "accounts-file") {
				t.Fatal("account file not marked live")
			}
			api := liveAccountManagement(t, c)
			snapshot := apiObject(t, api.call("GET", "/api/config", nil))
			live := false
			for _, raw := range snapshot["fields"].([]any) {
				field := raw.(map[string]any)
				if field["key"] == "accounts-file" {
					live = field["validation"].(map[string]any)["live"] == true
				}
			}
			if !live {
				t.Fatal("management form missing live badge")
			}
			save := func(path string, want int) map[string]any {
				t.Helper()
				before, _ := os.ReadFile(c.path)
				snapshot := apiObject(t, api.call("GET", "/api/config", nil))
				w := api.call("PUT", "/api/config", map[string]any{"fields": map[string]string{"accounts-file": path}, "revision": snapshot["revision"], "apply": false})
				if w.Code != want {
					t.Fatal(w.Code, w.Body.String())
				}
				if strings.Contains(w.Body.String(), "a-secret") {
					t.Fatal("password exposed")
				}
				if want != 200 {
					after, _ := os.ReadFile(c.path)
					if string(after) != string(before) {
						t.Fatal("failed load changed config")
					}
				}
				if c.worker != worker || c.generation != 1 {
					t.Fatal("account file save restarted proxy")
				}
				return apiObject(t, w)
			}
			endpoint := scheme + "://" + worker.listener.Addr().String() + "/tunnel"
			client := func(username, password string) *controller {
				t.Helper()
				return newController(t, "client", "server = "+endpoint+"\nusername = "+username+"\n"+ca, "-password", password)
			}
			legacy := client("", "app-test-password")
			old := onlineConnect(t, legacy.worker.listener.Addr().String(), "A")
			first := filepath.Join(filepath.Dir(c.path), "first.json")
			if err := os.WriteFile(first, []byte(`{"version":1,"accounts":[{"username":"alice","password":"a-secret"},{"username":"bob","password":"b-secret"}]}`), 0600); err != nil {
				t.Fatal(err)
			}
			result := save("first.json", 200)
			if result["applied"] == true || result["requires_restart"] != false || c.current.accountsFile != first {
				t.Fatal("first file did not load online", result)
			}
			old.SetReadDeadline(time.Now().Add(2 * time.Second))
			if _, err := old.Read(make([]byte, 1)); err == nil {
				t.Fatal("legacy stream survived account activation")
			}
			alice, bob := client("alice", "a-secret"), client("bob", "b-secret")
			one, two := onlineConnect(t, alice.worker.listener.Addr().String(), "A"), onlineConnect(t, bob.worker.listener.Addr().String(), "A")
			onlineExchange(t, one)
			onlineExchange(t, two)
			second := filepath.Join(filepath.Dir(c.path), "second.json")
			if err := os.WriteFile(second, []byte(`{"version":1,"accounts":[{"username":"alice","password":"new-secret"},{"username":"bob","password":"b-secret"}]}`), 0600); err != nil {
				t.Fatal(err)
			}
			save(second, 200)
			onlineExchange(t, two)
			one.SetReadDeadline(time.Now().Add(2 * time.Second))
			if _, err := one.Read(make([]byte, 1)); err == nil {
				t.Fatal("revoked account stream survived")
			}
			next := client("alice", "new-secret")
			onlineExchange(t, onlineConnect(t, next.worker.listener.Addr().String(), "A"))
			accounts, err := c.manageTunnelAccounts(nil)
			if err != nil || accounts.Path != second || accounts.Pending || len(accounts.Accounts) != 2 {
				t.Fatal("account manager retained old path", err)
			}
			save("missing.json", 400)
			broken := filepath.Join(filepath.Dir(c.path), "broken.json")
			if err := os.WriteFile(broken, []byte(`{"version":1,"accounts":null}`), 0600); err != nil {
				t.Fatal(err)
			}
			save(broken, 400)
			onlineExchange(t, two)
			// Unrelated saved target/password settings must remain pending during hot loading.
			setConfig(t, c, "target", "127.0.0.1:1")
			setConfig(t, c, "password", "pending-legacy-password")
			result = save("", 200)
			if c.current.server.Target != backend || c.current.server.Password != "app-test-password" || c.current.accountsFile != "" || result["requires_restart"] != true {
				t.Fatal("hot path applied pending cold settings", result)
			}
			restoredClient := client("", "app-test-password")
			restored := onlineConnect(t, restoredClient.worker.listener.Addr().String(), "A")
			onlineExchange(t, restored)
			if status := apiObject(t, api.call("GET", "/api/status", nil)); status["authenticated"] != true {
				t.Fatal("administrator session lost")
			}
		})
	}
}

func TestAccountsFileOnlineCLIOverrideAndMissingLegacyPassword(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "accounts.json")
	if err := os.WriteFile(file, []byte(`{"version":1,"accounts":[{"username":"alice","password":"a-secret"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	c := newController(t, "server", "target = localhost:80\naccounts-file = ''\n", "-accounts-file", file)
	setConfig(t, c, "accounts-file", "missing.json")
	if err := c.applyLive([]string{"accounts-file"}); err != nil || c.current.accountsFile != file {
		t.Fatal("CLI priority lost", err)
	}
	legacy := newController(t, "server", "target = localhost:80\naccounts-file = ''\n", "-accounts-file=")
	setConfig(t, legacy, "accounts-file", file)
	if err := legacy.applyLive([]string{"accounts-file"}); err != nil || legacy.current.accountsFile != "" {
		t.Fatal("CLI explicit empty ignored", err)
	}
	accountOnly := newController(t, "server", "target = localhost:80\naccounts-file = "+file+"\n", "-password=")
	setConfig(t, accountOnly, "accounts-file", "''")
	if err := accountOnly.applyLive([]string{"accounts-file"}); err == nil || accountOnly.current.accountsFile != file {
		t.Fatal("cleared authentication without valid legacy password")
	}
}
