package admin

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"sgate/internal/tunnel"
)

func TestTunnelAccountManagementAPIAuthAndTransactions(t *testing.T) {
	h, cfg := adminFixture(t)
	h.view.Role = "server"
	file := filepath.Join(filepath.Dir(h.view.AppConfigPath), "tunnel-accounts.json")
	original := []byte(`{"version":1,"accounts":[{"username":"alice","password":"private-original"}]}`)
	if err := os.WriteFile(file, original, 0600); err != nil {
		t.Fatal(err)
	}
	var fail atomic.Bool
	h.view.ManageTunnelAccounts = func(change *TunnelAccountChange) (TunnelAccountsSnapshot, error) {
		return ManageTunnelAccountsFile(file, change, func([]tunnel.Account) error {
			if fail.Load() {
				return errors.New("injected")
			}
			return nil
		})
	}
	cookie, csrf := setupAccount(t, h, cfg)
	if w := request(h, "GET", "/api/tunnel-accounts", nil, nil, "", ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := request(h, "PUT", "/api/tunnel-accounts", TunnelAccountChange{}, cookie, "", ""); w.Code != 403 {
		t.Fatal(w.Code)
	}
	if w := request(h, "GET", "/api/tunnel-accounts", nil, cookie, csrf, "http://evil.invalid"); w.Code != 403 {
		t.Fatal(w.Code)
	}
	read := func() TunnelAccountsSnapshot {
		t.Helper()
		w := request(h, "GET", "/api/tunnel-accounts", nil, cookie, csrf, "")
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "password") || strings.Contains(w.Body.String(), "private-original") {
			t.Fatal("account list leaked password")
		}
		var view TunnelAccountsSnapshot
		if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		return view
	}
	snapshot := read()
	create := TunnelAccountChange{Revision: snapshot.Revision, Action: "create", Username: "bob", Password: "new-private"}
	w := request(h, "PUT", "/api/tunnel-accounts", create, cookie, csrf, "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "new-private") {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request(h, "PUT", "/api/tunnel-accounts", create, cookie, csrf, ""); w.Code != 409 {
		t.Fatal("stale revision accepted", w.Code)
	}
	snapshot = read()
	disabled := true
	update := TunnelAccountChange{Revision: snapshot.Revision, Action: "update", Username: "alice", Disabled: &disabled}
	w = request(h, "PUT", "/api/tunnel-accounts", update, cookie, csrf, "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	accounts, err := tunnel.LoadAccounts(file)
	if err != nil || accounts[0].Password != "private-original" || !accounts[0].Disabled {
		t.Fatal("blank password failed to preserve existing credential", err)
	}
	before, _ := os.ReadFile(file)
	fail.Store(true)
	w = request(h, "PUT", "/api/tunnel-accounts", TunnelAccountChange{Revision: read().Revision, Action: "delete", Username: "alice"}, cookie, csrf, "")
	after, _ := os.ReadFile(file)
	if w.Code != 400 || string(before) != string(after) {
		t.Fatal("apply failure did not roll back", w.Code)
	}
	fail.Store(false)
	for _, name := range []string{"alice", "bob"} {
		w = request(h, "PUT", "/api/tunnel-accounts", TunnelAccountChange{Revision: read().Revision, Action: "delete", Username: name}, cookie, csrf, "")
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if accounts, err := tunnel.LoadAccounts(file); err != nil || len(accounts) != 0 {
		t.Fatal("last account could not be removed", err)
	}
	h.view.Role = "client"
	if w := request(h, "GET", "/api/tunnel-accounts", nil, cookie, csrf, ""); w.Code != 404 {
		t.Fatal("client exposed account management")
	}
}

func TestTunnelAccountConcurrentEditsHaveSingleWinner(t *testing.T) {
	h, cfg := adminFixture(t)
	h.view.Role = "server"
	file := filepath.Join(filepath.Dir(h.view.AppConfigPath), "accounts.json")
	raw := []byte(`{"version":1,"accounts":[]}`)
	os.WriteFile(file, raw, 0600)
	h.view.ManageTunnelAccounts = func(change *TunnelAccountChange) (TunnelAccountsSnapshot, error) {
		return ManageTunnelAccountsFile(file, change, func([]tunnel.Account) error { return nil })
	}
	cookie, csrf := setupAccount(t, h, cfg)
	var wg sync.WaitGroup
	var wins, conflicts atomic.Int32
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := request(h, "PUT", "/api/tunnel-accounts", TunnelAccountChange{Revision: digest(raw), Action: "create", Username: "alice", Password: "private-test"}, cookie, csrf, "")
			if w.Code == 200 {
				wins.Add(1)
			} else if w.Code == 409 {
				conflicts.Add(1)
			} else {
				t.Errorf("unexpected status %d", w.Code)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 || conflicts.Load() != 7 {
		t.Fatal("concurrent edits overwrote each other", wins.Load(), conflicts.Load())
	}
}
