package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sgate/internal/tunnel"
)

func TestAccountReverseMappingsEditAndSnapshot(t *testing.T) {
	mappings := []tunnel.ReverseMapping{{Listen: "127.0.0.1:10022", Target: "127.0.0.1:22"}}
	accounts, err := editTunnelAccounts(nil, &TunnelAccountChange{Action: "create", Username: "alice", Password: "secret", ReverseMappings: &mappings})
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts[0].ReverseMappings) != 1 {
		t.Fatal("create lost mappings")
	}
	accounts, err = editTunnelAccounts(accounts, &TunnelAccountChange{Action: "update", Username: "alice", Password: "new-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts[0].ReverseMappings) != 1 {
		t.Fatal("password edit lost mappings")
	}
	snapshot := accountSnapshot("", nil, accounts)
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "reverse_mappings") || strings.Contains(string(data), "new-secret") {
		t.Fatal("invalid public snapshot", string(data))
	}
	snapshot.Accounts[0].ReverseMappings[0].Target = "localhost:80"
	if accounts[0].ReverseMappings[0].Target != "127.0.0.1:22" {
		t.Fatal("snapshot aliases policy")
	}
	empty := []tunnel.ReverseMapping{}
	accounts, err = editTunnelAccounts(accounts, &TunnelAccountChange{Action: "update", Username: "alice", ReverseMappings: &empty})
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts[0].ReverseMappings) != 0 {
		t.Fatal("explicit empty array did not clear mappings")
	}
}

func TestReverseMappingsAccountAPITransactions(t *testing.T) {
	h, cfg := adminFixture(t)
	h.view.Role = "server"
	path := filepath.Join(filepath.Dir(h.view.AppConfigPath), "tunnel-accounts.json")
	original := []byte(`{"version":1,"accounts":[{"username":"alice","password":"private-original"}]}`)
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	var reject bool
	h.view.ManageTunnelAccounts = func(change *TunnelAccountChange) (TunnelAccountsSnapshot, error) {
		return ManageTunnelAccountsFile(path, change, func([]tunnel.Account) error {
			if reject {
				return errors.New("bind failed")
			}
			return nil
		})
	}
	cookie, csrf := setupAccount(t, h, cfg)
	read := func() TunnelAccountsSnapshot {
		t.Helper()
		w := request(h, "GET", "/api/tunnel-accounts", nil, cookie, csrf, "")
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "private-original") {
			t.Fatal("credential leaked")
		}
		var snapshot TunnelAccountsSnapshot
		if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	mappings := []tunnel.ReverseMapping{{Listen: "127.0.0.1:10022", Target: "127.0.0.1:22"}, {Listen: "[::1]:18080", Target: "intranet.example:8080"}}
	snapshot := read()
	save := func(values []tunnel.ReverseMapping, revision string) *httptest.ResponseRecorder {
		return request(h, "PUT", "/api/tunnel-accounts", TunnelAccountChange{Action: "update", Username: "alice", Revision: revision, ReverseMappings: &values}, cookie, csrf, "")
	}
	if w := save(mappings, snapshot.Revision); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	snapshot = read()
	if len(snapshot.Accounts[0].ReverseMappings) != 2 {
		t.Fatal("list omitted saved mappings")
	}
	disk, err := tunnel.LoadAccounts(path)
	if err != nil || len(disk[0].ReverseMappings) != 2 {
		t.Fatal("mappings were not persisted", err)
	}
	before, _ := os.ReadFile(path)
	invalid := []tunnel.ReverseMapping{{Listen: "localhost:10022", Target: "localhost:22"}}
	if w := save(invalid, snapshot.Revision); w.Code != 400 {
		t.Fatal("invalid mapping accepted", w.Code)
	}
	reject = true
	if w := save([]tunnel.ReverseMapping{}, snapshot.Revision); w.Code != 400 {
		t.Fatal("apply failure not reported", w.Code)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("failed mapping save changed account document")
	}
	reject = false
	if w := save([]tunnel.ReverseMapping{}, snapshot.Revision); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if len(read().Accounts[0].ReverseMappings) != 0 {
		t.Fatal("explicit empty array did not clear mappings")
	}
	if w := save(mappings, snapshot.Revision); w.Code != 409 {
		t.Fatal("stale mapping revision accepted", w.Code)
	}
}
