package admin

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func strptr(s string) *string { return &s }

func TestMappingFileMigrationSecretsAndRollback(t *testing.T) {
	h, cfg := adminFixture(t)
	original := []byte("service client {\nmaster_service = 127.0.0.1|9001\nlisten = 127.0.0.1:9000\nserver = ws://localhost/tunnel\nproxies = '[{\"listen\":\"127.0.0.1:9001\",\"server\":\"ws://localhost/tunnel\",\"username\":\"alice\",\"password\":\"alice-private\"}]'\n}\n")
	if err := os.WriteFile(h.view.AppConfigPath, original, 0600); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := setupAccount(t, h, cfg)
	h.view.LiveFields = []string{"proxies", "proxies-file", "master_service"}
	applied := 0
	fail := false
	h.view.ApplyLive = func(keys []string) error {
		if fail {
			return errors.New("injected apply failure")
		}
		applied++
		return nil
	}
	h.startupRevision = h.restartDigest(original)
	get := func() map[string]any {
		w := request(h, "GET", "/api/config", nil, cookie, "", "")
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "alice-private") || strings.Contains(w.Body.String(), "bob-private") {
			t.Fatal("mapping password leaked")
		}
		return object(t, w)
	}
	before := get()
	path := filepath.Join(filepath.Dir(h.view.AppConfigPath), "routes.json")
	save := func(fields map[string]string, mappings any, rev any) int {
		payload := map[string]any{"fields": fields, "revision": rev, "apply": false}
		if mappings != nil {
			payload["proxies"] = mappings
		}
		w := request(h, "PUT", "/api/config", payload, cookie, csrf, "")
		if w.Code != 200 && w.Code != 409 {
			t.Log(w.Body.String())
		}
		return w.Code
	}
	if code := save(map[string]string{"proxies-file": "routes.json"}, nil, before["revision"]); code != 200 || applied != 1 {
		t.Fatal("migration failed", code, applied)
	}
	raw, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(raw), "alice-private") {
		t.Fatal("credentials not migrated", err)
	}
	if st, err := os.Stat(path); err != nil || runtime.GOOS != "windows" && st.Mode().Perm() != 0600 {
		t.Fatal("mapping file not private")
	}
	config, _ := os.ReadFile(h.view.AppConfigPath)
	if !strings.Contains(string(config), `proxies-file = "routes.json"`) || strings.Contains(string(config), ".sgate-mappings-") {
		t.Fatal("staging path persisted")
	}
	after := get()
	mappings := []proxyMapping{{Listen: "127.0.0.1:9001", Server: "ws://localhost/tunnel", Username: strptr("alice"), Password: strptr(secretMarker)}, {Listen: "127.0.0.1:9002", Server: "ws://localhost/tunnel", Username: strptr("bob"), Password: strptr("bob-private")}}
	if code := save(map[string]string{}, mappings, after["revision"]); code != 200 {
		t.Fatal("mapping update failed", code)
	}
	updated, _ := os.ReadFile(path)
	if !strings.Contains(string(updated), "alice-private") || !strings.Contains(string(updated), "bob-private") || strings.Contains(string(updated), secretMarker) {
		t.Fatal("password not preserved")
	}
	config, _ = os.ReadFile(h.view.AppConfigPath)
	if !strings.Contains(string(config), "127.0.0.1|9002") {
		t.Fatal("framework listeners not synchronized")
	}
	current := get()
	fail = true
	mappings[1].Password = strptr("changed-private")
	if code := save(map[string]string{}, mappings, current["revision"]); code != 409 {
		t.Fatal("failed apply accepted", code)
	}
	restored, _ := os.ReadFile(path)
	restoredConfig, _ := os.ReadFile(h.view.AppConfigPath)
	if string(restored) != string(updated) || string(restoredConfig) != string(config) {
		t.Fatal("two-file rollback failed")
	}
	fail = false
	// The .cf is unchanged, but an external JSON edit must invalidate its page revision.
	if err := os.WriteFile(path, append(updated, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if code := save(map[string]string{}, mappings, current["revision"]); code != 409 {
		t.Fatal("stale JSON revision accepted", code)
	}
	current = get()
	// Turning file mode off migrates current mappings back to the inline setting.
	if code := save(map[string]string{"proxies-file": ""}, nil, current["revision"]); code != 200 {
		t.Fatal("inline migration failed", code)
	}
	config, _ = os.ReadFile(h.view.AppConfigPath)
	restoredMappings, storedPath, err := mappingConfig(config)
	if err != nil || storedPath != "" || len(restoredMappings) != 2 || *restoredMappings[0].Password != "alice-private" {
		t.Fatal("inline credentials not restored", err)
	}
	get()
	staged, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".sgate-*"))
	if len(staged) != 0 {
		t.Fatal("staging files leaked", staged)
	}
}

func TestMappingFileFirstSaveFailureRemovesNewFile(t *testing.T) {
	h, cfg := adminFixture(t)
	cookie, csrf := setupAccount(t, h, cfg)
	original, _ := os.ReadFile(h.view.AppConfigPath)
	before := object(t, request(h, "GET", "/api/config", nil, cookie, "", ""))
	h.view.Apply = func() error { return errors.New("injected failure") }
	w := request(h, "PUT", "/api/config", map[string]any{"fields": map[string]string{"proxies-file": "new.json"}, "revision": before["revision"], "apply": true}, cookie, csrf, "")
	if w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	after, _ := os.ReadFile(h.view.AppConfigPath)
	if string(original) != string(after) {
		t.Fatal("old config changed")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(h.view.AppConfigPath), "new.json")); !os.IsNotExist(err) {
		t.Fatal("new file survived rollback", err)
	}
}

func TestInlineMappingSecretsRoundTripWithoutEcho(t *testing.T) {
	original := []byte("proxies = '[{\"listen\":\"127.0.0.1:9001\",\"server\":\"ws://localhost/tunnel\",\"password\":\"private-value\"}]'\n")
	redacted := redactConfig(original)
	if strings.Contains(redacted, "private-value") {
		t.Fatal("inline password exposed")
	}
	restored, err := restoreSecret(redacted, original)
	if err != nil || !strings.Contains(restored, "private-value") || strings.Contains(restored, secretMarker) {
		t.Fatal("masked password did not roundtrip", err)
	}
}
