package app

import (
	"path/filepath"
	"sgate/internal/admin"
	"sgate/internal/cli"
	"testing"
)

func TestWindowsStandaloneSampleLoadsProxyAndAdminSettings(t *testing.T) {
	path := filepath.Join("..", "..", "samples", "windows-client.cf")
	fs := appFlags("client")
	fs.Bool("admin-enabled", false, "")
	if err := cli.Parse(fs, []string{"-config", path}); err != nil {
		t.Fatal(err)
	}
	if fs.Lookup("master_service").Value.String() != "127.0.0.1|9000" || fs.Lookup("vpn-mode").Value.String() != "disabled" || fs.Lookup("admin-enabled").Value.String() != "true" {
		t.Fatal("unsafe Windows sample defaults")
	}
	cfg, err := admin.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:9081" || cfg.WebDir == "" {
		t.Fatal("Windows sample has no management UI")
	}
}
