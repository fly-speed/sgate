package app

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"sgate/internal/cli"
	"sgate/internal/proxyconfig"
)

func startupClient(t *testing.T, c *controller) (*Client, error) {
	t.Helper()
	fs := appFlags("client")
	if err := cli.Parse(fs, c.args); err != nil {
		return nil, err
	}
	return NewClient(context.Background(), fs, c.args, "")
}

func TestStartupInitializesMissingProxyFile(t *testing.T) {
	for _, inline := range []bool{false, true} {
		name := "empty"
		if inline {
			name = "inline-seed"
		}
		t.Run(name, func(t *testing.T) {
			text := "proxies-file = nested/routes.json\n"
			if inline {
				text += `proxies = '[{"listen":"127.0.0.1:19001","server":"ws://localhost/tunnel","username":"alice","password":"private-test-secret"}]'` + "\n"
			}
			c := proxyConfigController(t, text)
			path := filepath.Join(filepath.Dir(c.path), "nested", "routes.json")
			if _, err := c.load(""); err == nil {
				t.Fatal("read-only validation unexpectedly succeeded")
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("validation created mapping storage")
			}
			client, err := startupClient(t, c)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			data, err := proxyconfig.Read(path)
			if err != nil {
				t.Fatal(err)
			}
			mappings, err := proxyconfig.Decode(data)
			if err != nil {
				t.Fatal(err)
			}
			if inline {
				if len(mappings) != 1 || *mappings[0].Username != "alice" || *mappings[0].Password != "private-test-secret" || len(client.initial.proxies) != 1 {
					t.Fatal("inline settings not migrated")
				}
			} else if strings.TrimSpace(string(data)) != "[]" || client.initial.client == nil {
				t.Fatal("empty initialization must retain an idle common template")
			}
			st, _ := os.Stat(path)
			if runtime.GOOS != "windows" && st.Mode().Perm() != 0600 {
				t.Fatal("new file not private")
			}
			// Repeated startup reads rather than replacing the persisted document.
			again, err := startupClient(t, c)
			if err != nil {
				t.Fatal(err)
			}
			again.Close()
			after, _ := os.ReadFile(path)
			if string(after) != string(data) {
				t.Fatal("restart overwrote mapping file")
			}
		})
	}
}

func TestStartupProxyFileCLIAndExistingInvalidFile(t *testing.T) {
	c := proxyConfigController(t, testClientMapping(t, "ws://localhost/tunnel")+"proxies-file = ignored.json\n", "-proxies=")
	client, err := startupClient(t, c)
	if err != nil {
		t.Fatal(err)
	}
	client.Close()
	ignored := filepath.Join(filepath.Dir(c.path), "ignored.json")
	if _, err := os.Stat(ignored); !os.IsNotExist(err) {
		t.Fatal("CLI inline override still created file")
	}
	absolute := filepath.Join(t.TempDir(), "absolute.json")
	c.args = append(c.args, "-proxies-file", absolute)
	client, err = startupClient(t, c)
	if err != nil {
		t.Fatal(err)
	}
	client.Close()
	if _, err := os.Stat(absolute); err != nil {
		t.Fatal("absolute CLI path not created", err)
	}
	if _, err := os.Stat(ignored); !os.IsNotExist(err) {
		t.Fatal("overridden file created")
	}
	if err := os.WriteFile(absolute, []byte("invalid-existing-data"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := startupClient(t, c); err == nil {
		t.Fatal("existing invalid file ignored")
	}
	after, _ := os.ReadFile(absolute)
	if string(after) != "invalid-existing-data" {
		t.Fatal("existing invalid file overwritten")
	}
}

func TestStartupInvalidSeedDoesNotCreateProxyFile(t *testing.T) {
	c := proxyConfigController(t, `proxies-file = new/routes.json
proxies = '[{"listen":"127.0.0.1:19001","server":"http://localhost/tunnel","username":"alice","password":"private-test-secret"}]'
`)
	_, err := startupClient(t, c)
	if err == nil || strings.Contains(err.Error(), "private-test-secret") {
		t.Fatal("invalid seed accepted or password exposed", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(c.path), "new")); !os.IsNotExist(err) {
		t.Fatal("invalid seed left storage")
	}
}
