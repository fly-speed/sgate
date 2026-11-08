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
func TestClientRejectsRemovedServerOption(t *testing.T) {
	for _, content := range []string{"server = ws://localhost/tunnel\n", "server = ''\n", "service client {\nserver = ws://localhost/tunnel\n}\n"} {
		path := filepath.Join(t.TempDir(), "client.cf")
		os.WriteFile(path, []byte(content), 0600)
		if err := cli.Parse(appFlags("client"), []string{"-config", path}); err == nil {
			t.Fatal("removed server option accepted")
		}
	}
	if err := cli.Parse(appFlags("client"), []string{"-server", "ws://localhost/tunnel"}); err == nil {
		t.Fatal("removed -server accepted")
	}
}
