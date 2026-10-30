package admin

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"sgate/internal/tunnel"
)

func TestFirstTunnelAccountCreatesConfigAndPreservesPendingSettings(t *testing.T) {
	h, cfg := adminFixture(t)
	h.view.Role = "server"
	h.view.ApplyLive = func([]string) error { return nil }
	original, _ := os.ReadFile(h.view.AppConfigPath)
	h.startupContent = original
	h.startupRevision = h.restartDigest(original)
	// Disk has another saved, unapplied parameter. Bootstrap must not mark it applied.
	pending := append(append([]byte{}, original...), []byte("target = localhost:99\n")...)
	if err := os.WriteFile(h.view.AppConfigPath, pending, 0600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(filepath.Dir(h.view.AppConfigPath), "tunnel-accounts.json")
	h.view.ManageTunnelAccounts = func(change *TunnelAccountChange) (TunnelAccountsSnapshot, error) {
		return SetupTunnelAccounts(h.view.AppConfigPath, file, "", change, func(string) error { return nil }, func([]tunnel.Account) error { return nil })
	}
	cookie, csrf := setupAccount(t, h, cfg)
	snapshot, err := h.view.ManageTunnelAccounts(nil)
	if err != nil || !snapshot.CanCreate {
		t.Fatal("bootstrap unavailable", err)
	}
	w := request(h, "PUT", "/api/tunnel-accounts", TunnelAccountChange{Revision: snapshot.Revision, Action: "create", Username: "alice", Password: "private-first-secret"}, cookie, csrf, "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "private-first-secret") {
		t.Fatal("first password leaked")
	}
	accounts, err := tunnel.LoadAccounts(file)
	if err != nil || len(accounts) != 1 {
		t.Fatal("JSON was not created", err)
	}
	data, _ := os.ReadFile(h.view.AppConfigPath)
	if !strings.Contains(string(data), "accounts-file") || !strings.Contains(string(data), "target = localhost:99") {
		t.Fatal("config not preserved")
	}
	if h.restartDigest(data) == h.startupRevision {
		t.Fatal("bootstrap marked an unrelated pending setting as applied")
	}
	// Reverting just the unrelated pending setting should leave no restart pending.
	reverted := strings.Replace(string(data), "target = localhost:99\n", "", 1)
	if h.restartDigest([]byte(reverted)) != h.startupRevision {
		t.Fatal("bootstrap account path was not recorded as applied")
	}
}

func TestFirstTunnelAccountRollbackAndNonClobber(t *testing.T) {
	for _, mode := range []string{"validate-failure", "apply-failure", "existing-file", "blocked-cli", "invalid-account", "merged-config"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			config := filepath.Join(dir, "server.cf")
			file := filepath.Join(dir, "accounts.json")
			original := []byte("password = old-secret\naccounts-file = ''\n")
			if mode == "merged-config" {
				original = []byte("service sgate {\n password = old-secret\n}\n")
			}
			if err := os.WriteFile(config, original, 0600); err != nil {
				t.Fatal(err)
			}
			validate := func(string) error { return nil }
			apply := func([]tunnel.Account) error { return nil }
			blocked := ""
			if mode == "validate-failure" {
				validate = func(string) error { return errors.New("invalid config") }
			}
			if mode == "apply-failure" {
				apply = func([]tunnel.Account) error { return errors.New("proxy stopped") }
			}
			if mode == "existing-file" {
				os.WriteFile(file, []byte("keep existing"), 0600)
			}
			if mode == "blocked-cli" {
				blocked = "CLI disabled"
			}
			snapshot, err := SetupTunnelAccounts(config, file, blocked, nil, validate, apply)
			if err != nil {
				t.Fatal(err)
			}
			password := "new-secret"
			if mode == "invalid-account" {
				password = ""
			}
			result, err := SetupTunnelAccounts(config, file, blocked, &TunnelAccountChange{Action: "create", Username: "alice", Password: password, Revision: snapshot.Revision}, validate, apply)
			if mode == "merged-config" {
				if err != nil || !result.Enabled {
					t.Fatal(err)
				}
				data, _ := os.ReadFile(config)
				if strings.Index(string(data), "accounts-file") > strings.Index(string(data), "}") {
					t.Fatal("field appended outside service block")
				}
				return
			}
			if err == nil {
				t.Fatal("failed bootstrap reported success")
			}
			data, _ := os.ReadFile(config)
			if string(data) != string(original) {
				t.Fatal("original config was changed")
			}
			data, readErr := os.ReadFile(file)
			if mode == "existing-file" {
				if string(data) != "keep existing" {
					t.Fatal("existing file overwritten")
				}
			} else if !os.IsNotExist(readErr) {
				t.Fatal("failed bootstrap left account file behind")
			}
		})
	}
}

func TestConcurrentFirstTunnelAccountDoesNotOverwriteWinner(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "server.cf")
	file := filepath.Join(dir, "accounts.json")
	raw := []byte("password = old-secret\n")
	os.WriteFile(config, raw, 0600)
	var wg sync.WaitGroup
	var wins atomic.Int32
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := SetupTunnelAccounts(config, file, "", &TunnelAccountChange{Action: "create", Username: "alice", Password: "secret", Revision: digest(raw)}, func(string) error { return nil }, func([]tunnel.Account) error { return nil })
			if err == nil {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatal("concurrent bootstrap lost winner", wins.Load())
	}
	if _, err := tunnel.LoadAccounts(file); err != nil {
		t.Fatal("winner's account file missing", err)
	}
}
