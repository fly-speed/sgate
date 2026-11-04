package desktop

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sgate/internal/clientcontrol"
)

func TestMain(m *testing.M) {
	if os.Getenv("SGATE_CONTROL_TEST_HELPER") == "1" && os.Getenv(clientcontrol.Environment) == "1" {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		defer clientcontrol.Watch(ctx, cancel, os.Stdin)()
		fmt.Println("ready")
		<-ctx.Done()
		time.Sleep(60 * time.Millisecond) // Represents network cleanup before exit.
		if err := os.WriteFile(os.Getenv("SGATE_CONTROL_TEST_MARKER"), []byte("clean"), 0600); err != nil {
			os.Exit(2)
		}
		return
	}
	os.Exit(m.Run())
}
func TestControllerStartStopWaitsForClientCleanupAndSupportsRestart(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "cleanup")
	t.Setenv("SGATE_CONTROL_TEST_HELPER", "1")
	t.Setenv("SGATE_CONTROL_TEST_MARKER", marker)
	c := New(dir)
	c.Config = filepath.Join(dir, "sgate-client.cf")
	os.WriteFile(c.Config, []byte("service client {\nadmin_enabled = true\n}"), 0600)
	c.Executable, _ = os.Executable()
	for range 2 {
		os.Remove(marker)
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(c.Stop)
		if err := c.Start(); err == nil {
			t.Fatal("duplicate launch allowed")
		}
		if c.Snapshot().PID <= 0 {
			t.Fatal("missing process PID")
		}
		c.Stop()
		if state := c.Snapshot().State; state != "stopping" {
			t.Fatal("claimed shutdown before cleanup", state)
		}
		deadline := time.Now().Add(5 * time.Second)
		for c.Snapshot().State == "stopping" && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if s := c.Snapshot(); s.State != "stopped" || s.Error != "" {
			t.Fatal(s)
		}
		if data, e := os.ReadFile(marker); e != nil || string(data) != "clean" {
			t.Fatal("cleanup was not completed", e)
		}
	}
}
func TestControllerFailuresAreVisible(t *testing.T) {
	c := New(t.TempDir())
	if c.Start() == nil || c.Snapshot().State != "failed" {
		t.Fatal("missing executable ignored")
	}
	c.Stop()
}
func TestManagementURLUsesConfiguredListenerAndTLS(t *testing.T) {
	for _, tc := range []struct{ config, want string }{
		{"admin_enabled = true\nadmin_listen = 0.0.0.0:9081", "http://127.0.0.1:9081/"},
		{"admin_enabled = true\nadmin_listen = '[::]:9443'\nadmin_tls_cert = test.pem", "https://[::1]:9443/"},
	} {
		c := New(t.TempDir())
		os.MkdirAll(filepath.Dir(c.Config), 0700)
		os.WriteFile(c.Config, []byte("service client {\n"+tc.config+"\n}"), 0600)
		got, err := c.ManagementURL()
		if err != nil || got != tc.want {
			t.Fatal(got, err)
		}
	}
	c := New(t.TempDir())
	os.MkdirAll(filepath.Dir(c.Config), 0700)
	os.WriteFile(c.Config, []byte("service client {\nadmin_enabled = true\nadmin_listen = invalid\n}"), 0600)
	if _, err := c.ManagementURL(); err == nil || !strings.Contains(err.Error(), "admin_listen") {
		t.Fatal("invalid listener accepted", err)
	}
}
