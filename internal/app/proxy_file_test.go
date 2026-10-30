package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func credential(s string) *string { return &s }

func TestProxyFilePathsCredentialsAndCLI(t *testing.T) {
	c := proxyConfigController(t, "server = ws://localhost/tunnel\nusername = common-user\nproxies-file = routes.json\n")
	path := filepath.Join(filepath.Dir(c.path), "routes.json")
	routes := []clientProxy{{Listen: "127.0.0.1:19001", Server: "ws://localhost/tunnel", Username: credential("alice"), Password: credential("alice-secret")}, {Listen: "127.0.0.1:19002", Server: "wss://localhost/tunnel"}}
	raw, _ := json.Marshal(routes)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	next, err := c.load("")
	if err != nil {
		t.Fatal(err)
	}
	if next.proxies[0].config.Username != "alice" || next.proxies[0].config.Password != "alice-secret" || next.proxies[1].config.Username != "common-user" || next.proxies[1].config.Password != "multi-test-password" {
		t.Fatal("mapping credentials not isolated or inherited")
	}
	setConfig(t, c, "proxies-file", path)
	if _, err = c.load(""); err != nil {
		t.Fatal("absolute path failed", err)
	}
	c.args = append(c.args, "-proxies", `[{"listen":"127.0.0.1:19003","server":"ws://localhost/tunnel"}]`)
	next, err = c.load("")
	if err != nil || len(next.proxies) != 1 || next.proxies[0].port != "19003" {
		t.Fatal("CLI inline override lost", err)
	}
	c.args = append(c.args, "-proxies-file", path)
	next, err = c.load("")
	if err != nil || len(next.proxies) != 2 {
		t.Fatal("explicit file override lost", err)
	}
	if err = os.WriteFile(path, []byte("[]"), 0600); err != nil {
		t.Fatal(err)
	}
	next, err = c.load("")
	if err != nil || next.client == nil || len(next.proxies) != 0 {
		t.Fatal("empty file single-proxy fallback failed", err)
	}
	for _, raw := range []string{"", "null", "[{}]", `[{"listen":"127.0.0.1:19001","server":"ws://localhost/tunnel","password":""}]`, `[{"listen":"127.0.0.1:19001","server":"ws://localhost/tunnel","username":"bad user"}]`, "[] {}"} {
		if err = os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err = c.load(""); err == nil {
			t.Fatal("invalid file accepted")
		}
	}
}

func TestProxyFileCandidateRespectsCLIAndStaging(t *testing.T) {
	c := proxyConfigController(t, "proxies-file = routes.json\n")
	path := filepath.Join(filepath.Dir(c.path), "routes.json")
	staged := filepath.Join(filepath.Dir(c.path), ".sgate-mappings-test.json")
	raw := []byte(`[{"listen":"127.0.0.1:19001","server":"ws://localhost/tunnel"}]`)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte(`[{"listen":"127.0.0.1:19002","server":"ws://localhost/tunnel","username":"alice","password":"test-secret"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	c.args = append(c.args, "-proxies-file", path)
	addresses, err := c.mappingCandidate(c.path, path, staged)
	if err != nil || len(addresses) != 1 || addresses[0] != "127.0.0.1:19002" {
		t.Fatal("staged CLI file not used for validation/listeners", addresses, err)
	}
	actual, err := c.load("")
	if err != nil || actual.proxies[0].Listen != "127.0.0.1:19001" {
		t.Fatal("validation changed running file", err)
	}
}
