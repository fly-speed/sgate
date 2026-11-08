package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVPNOptionsConfigAndCLIOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vpn.conf")
	if err := os.WriteFile(path, []byte("role = server\nlisten = 127.0.0.1:8444\nmanage-network = false\nmtu = 1200\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := parse([]string{"-config", path, "-mtu", "1280"})
	if err != nil || got.role != "server" || got.manage || got.mtu != 1280 {
		t.Fatal(got, err)
	}
	if _, err := parse([]string{"-role", "unknown"}); err == nil {
		t.Fatal("unknown role allowed")
	}
	if err := os.WriteFile(path, []byte("mtu = 1200\nmtu = 1280\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := parse([]string{"-config", path}); err == nil {
		t.Fatal("duplicate config accepted")
	}
}

func TestVPNClientToClientOption(t *testing.T) {
	defaults, err := parse(nil)
	if err != nil || defaults.clientToClient {
		t.Fatal("client communication enabled by default", err)
	}
	path := filepath.Join(t.TempDir(), "vpn.conf")
	if err := os.WriteFile(path, []byte("role = server\nallow-client-to-client = true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	enabled, err := parse([]string{"-config", path})
	if err != nil || !enabled.clientToClient {
		t.Fatal(enabled, err)
	}
	disabled, err := parse([]string{"-config", path, "-allow-client-to-client=false"})
	if err != nil || disabled.clientToClient {
		t.Fatal(disabled, err)
	}
}
