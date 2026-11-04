package admin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sgate/internal/tunnel"
)

func TestVPNAccountsWebManagement(t *testing.T) {
	for _, role := range []string{"server", "client"} {
		t.Run(role, func(t *testing.T) {
			h, cfg := adminFixture(t)
			h.view.Role = role
			h.view.ManageVPNAccounts = func(change *VPNAccountChange) (VPNAccountsSnapshot, error) {
				return ManageVPNAccountFile(h.view.AppConfigPath, role, "", nil, change)
			}
			const endpoint = "/api/vpn-accounts"
			if w := request(h, "GET", endpoint, nil, nil, "", ""); w.Code != 401 {
				t.Fatal(w.Code)
			}
			cookie, csrf := setupAccount(t, h, cfg)
			if w := request(h, "PUT", endpoint, VPNAccountChange{}, cookie, "", ""); w.Code != 403 {
				t.Fatal(w.Code)
			}
			view := func(method string, change *VPNAccountChange) VPNAccountsSnapshot {
				t.Helper()
				w := request(h, method, endpoint, change, cookie, csrf, "")
				if w.Code != 200 || strings.Contains(w.Body.String(), "private-secret") || strings.Contains(w.Body.String(), "password") {
					t.Fatal(w.Code, w.Body.String())
				}
				var snapshot VPNAccountsSnapshot
				if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil {
					t.Fatal(err)
				}
				return snapshot
			}
			before := view("GET", nil)
			action := "create"
			if role == "client" {
				action = "save"
			}
			change := &VPNAccountChange{Revision: before.Revision, Action: action, Username: "alice", Password: "private-secret"}
			saved := view("PUT", change)
			config, _ := os.ReadFile(h.view.AppConfigPath)
			if !saved.ConfigChanged || !strings.Contains(string(config), VPNAccountFileKey(role)) ||
				strings.Contains(string(config), change.Password) || !strings.Contains(string(config), "existing-proxy-secret") {
				t.Fatal("bootstrap failed to preserve separate configuration")
			}
			if w := request(h, "PUT", endpoint, change, cookie, csrf, ""); w.Code != 409 {
				t.Fatal("stale revision accepted", w.Code)
			}
			disabled := true
			edit := &VPNAccountChange{Revision: saved.Revision, Action: "update", Username: "alice", NewUsername: "bob", Disabled: &disabled}
			if role == "client" {
				edit = &VPNAccountChange{Revision: saved.Revision, Action: "save", Username: "bob"}
			}
			saved = view("PUT", edit)
			accounts, err := tunnel.LoadAccounts(saved.Path)
			if err != nil || len(accounts) != 1 || accounts[0].Username != "bob" || accounts[0].Password != change.Password || accounts[0].Disabled != (role == "server") {
				t.Fatal("stored account differs from the edit", err)
			}
			if role == "server" {
				deleted := view("PUT", &VPNAccountChange{Revision: saved.Revision, Action: "delete", Username: "bob"})
				if len(deleted.Accounts) != 0 {
					t.Fatal("account was not deleted")
				}
			}
		})
	}
}

func TestVPNAccountsRejectsProxyFileAlias(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "server.cf")
	proxy := filepath.Join(dir, "proxy.json")
	alias := filepath.Join(dir, "vpn.json")
	if err := os.WriteFile(config, []byte("accounts-file = proxy.json\nvpn-accounts-file = vpn.json\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(proxy, []byte(`{"version":1,"accounts":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(proxy, alias); err != nil {
		t.Fatal(err)
	}
	view, err := ManageVPNAccountFile(config, "server", "", nil, nil)
	if err != nil || view.Blocked == "" {
		t.Fatal("proxy file alias allowed", err)
	}
}
