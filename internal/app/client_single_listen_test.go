package app

import (
	"os"
	"path/filepath"
	"testing"

	"sgate/internal/cli"
)

func TestEmptyProxyFileNeverCreatesImplicitRoute(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.json")
	os.WriteFile(path, []byte("[]\n"), 0600)
	c := proxyConfigController(t, "master_service = 127.0.0.1|1982\nproxies-file = "+path+"\n")
	d, err := c.load("")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.clientRoutes(d.listen)) != 0 || d.client.URL != "" {
		t.Fatal("empty mappings created an implicit route")
	}
}
func TestClientIgnoresRemovedFileOptionsWithoutCreatingRoutes(t *testing.T) {
	for _, merged := range []bool{false, true} {
		for _, content := range []string{"server = ws://localhost/tunnel\n", "server = ''\n", "listen = 127.0.0.1:1\nserver = ws://localhost/tunnel\nfuture-setting = ignored\n"} {
			c := proxyConfigController(t, content)
			if merged {
				raw, err := os.ReadFile(c.path)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(c.path, []byte("service client {\n"+string(raw)+"}\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			d, err := c.load("")
			if err != nil {
				t.Fatal(err)
			}
			if len(d.clientRoutes(d.listen)) != 0 || d.client.URL != "" {
				t.Fatal("ignored server option created a legacy proxy route")
			}
		}
	}

	if err := cli.Parse(appFlags("client"), []string{"-server", "ws://localhost/tunnel"}); err == nil {
		t.Fatal("removed -server accepted")
	}
}
