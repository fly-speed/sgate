package admin

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestServiceControlPathsResolveBesideManagementConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "service.cf")
	content := "service sgate {\nadmin_master_ctl = ../bin/master_ctl\nadmin_master_socket = ../run/master.sock\n}\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	configPath, err := filepath.Rel(cwd, path)
	if err != nil && runtime.GOOS == "windows" && filepath.VolumeName(cwd) != filepath.VolumeName(path) {
		configPath, err = path, nil
	}
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MasterCtl != filepath.Clean(filepath.Join(dir, "../bin/master_ctl")) ||
		cfg.MasterSocket != filepath.Clean(filepath.Join(dir, "../run/master.sock")) {
		t.Fatal("control paths did not resolve relative to the management configuration", cfg)
	}
}

func TestServiceRestartRequiresSessionAndCSRF(t *testing.T) {
	h, cfg := adminFixture(t)
	cookie, csrf := setupAccount(t, h, cfg)
	h.cfg.MasterCtl, h.cfg.MasterSocket = "/test/master_ctl", "/test/master.sock"
	calls := 0
	h.view.RestartService = func(control, socket string, expectedPID int) error {
		calls++
		if control != h.cfg.MasterCtl || socket != h.cfg.MasterSocket || expectedPID != os.Getpid() {
			t.Fatal("restart did not use startup management settings")
		}
		return nil
	}
	for _, check := range []struct {
		cookie       *http.Cookie
		csrf, origin string
		want         int
	}{
		{nil, "", "", 401},
		{cookie, "", "", 403},
		{cookie, csrf, "https://untrusted.example", 403},
	} {
		w := request(h, "POST", "/api/restart-service", nil, check.cookie, check.csrf, check.origin)
		if w.Code != check.want {
			t.Fatalf("got %d, want %d", w.Code, check.want)
		}
	}
	if calls != 0 {
		t.Fatal("unauthorized request invoked restart")
	}
	if w := request(h, "POST", "/api/restart-service", map[string]int{}, cookie, csrf, ""); w.Code != 400 {
		t.Fatal("restart without a target process was accepted", w.Code)
	}
	if w := request(h, "POST", "/api/restart-service", map[string]any{
		"expected_pid": os.Getpid(), "command": "not allowed",
	}, cookie, csrf, ""); w.Code != 400 || calls != 0 {
		t.Fatal("arbitrary command field was accepted", w.Code, calls)
	}
	w := request(h, "POST", "/api/restart-service", map[string]int{"expected_pid": os.Getpid()}, cookie, csrf, "")
	if w.Code != 202 || calls != 1 || object(t, w)["restart_requested"] != true {
		t.Fatal(w.Code, w.Body.String(), calls)
	}
	h.view.RestartService = func(string, string, int) error { return errors.New("controller unavailable") }
	if w = request(h, "POST", "/api/restart-service", map[string]int{"expected_pid": os.Getpid()}, cookie, csrf, ""); w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	h.view.RestartService = nil
	if w = request(h, "POST", "/api/restart-service", map[string]int{"expected_pid": os.Getpid()}, cookie, csrf, ""); w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
}
