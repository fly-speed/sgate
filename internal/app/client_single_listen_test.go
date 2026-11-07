package app

import (
	"os"
	"path/filepath"
	"testing"

	"sgate/internal/tunnel"
)

func TestEmptyProxyFileUsesMasterService(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.json")
	if err := os.WriteFile(path, []byte("[]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{"|1982", "0.0.0.0|1982"} {
		c := proxyConfigController(t, "master_service = "+address+"\nserver = ws://localhost:8081/tunnel\nproxies-file = "+path+"\n")
		d, err := c.load("")
		if err != nil {
			t.Fatal(err)
		}
		routes := d.clientRoutes(d.listen)
		if d.client == nil || len(d.proxies) != 0 || len(routes) != 1 || routes[0].port != "1982" {
			t.Fatal(address, d.listen, routes)
		}
		if err := validateProxyListeners(routes, []string{"127.0.0.1:1982", "192.168.1.28:1982"}); err != nil {
			t.Fatal(err)
		}
		c.args = append(c.args, "-master_service", "127.0.0.1|1983")
		d, err = c.load("")
		if err != nil || d.listen != "127.0.0.1:1983" {
			t.Fatal("CLI override lost", d.listen, err)
		}
	}
}

func TestSingleProxyRoutesShareBackendAcrossMasterListeners(t *testing.T) {
	d := definition{listen: "127.0.0.1:1982, [::1]:1982, 127.0.0.1:1983", client: &tunnel.ClientConfig{URL: "wss://localhost:8081/tunnel"}}
	routes := d.clientRoutes(d.listen)
	if len(routes) != 2 || routes[0].port != "1982" || routes[1].port != "1983" || routes[0].Server != routes[1].Server {
		t.Fatal(routes)
	}
}
