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
