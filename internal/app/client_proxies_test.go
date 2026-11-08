package app

import (
	"context"
	"os"
	"path/filepath"
	"sgate/internal/tunnel"
	"strings"
	"testing"
)

func TestMultiProxyConfigAndCLI(t *testing.T) {
	raw := `[{"listen":"127.0.0.1:19001","server":"ws://localhost:8080/tunnel?addr=127.0.0.1:22"},{"listen":"127.0.0.1:19002","server":"wss://localhost:8081/tunnel","addr":"localhost:8088"}]`
	c := proxyConfigController(t, "proxies = '"+raw+"'\naddr = ignored.example:1234\nnoise-interval-min = 10ms\nnoise-interval-max = 30ms\n")
	d, err := c.load("")
	if err != nil {
		t.Fatal(err)
	}
	if d.listen != "127.0.0.1:19001,127.0.0.1:19002" || len(d.proxies) != 2 {
		t.Fatal(d.listen, d.proxies)
	}
	if d.proxies[0].config.Addr != "" || d.proxies[1].config.Addr != "localhost:8088" || !strings.Contains(d.proxies[0].config.URL, "127.0.0.1:22") {
		t.Fatal("mapping target was overridden")
	}
	if d.proxies[0].config.Options.NoiseIntervalMin == 0 {
		t.Fatal("common options not inherited")
	}
	if err := validateProxyListeners(d.proxies, []string{"127.0.0.1:19002", "127.0.0.1:19001"}); err != nil {
		t.Fatal(err)
	}
	wildcardAddresses := []string{
		"127.0.0.1:19001", "192.0.2.1:19001",
		"127.0.0.1:19002", "192.0.2.1:19002",
	}
	if err := validateProxyListeners(d.proxies, wildcardAddresses); err != nil {
		t.Fatal("wildcard listeners on multiple interfaces rejected:", err)
	}
	for _, addresses := range [][]string{{"127.0.0.1:19001"}, {"127.0.0.1:19001", "127.0.0.1:19003"}, {"127.0.0.1:19001", "127.0.0.1:19001"}} {
		if validateProxyListeners(d.proxies, addresses) == nil {
			t.Fatal("inherited listener mismatch accepted", addresses)
		}
	}
	replacement := `[{"listen":"127.0.0.1:19003","server":"ws://localhost:8082/tunnel"}]`
	c = proxyConfigController(t, "proxies = '"+raw+"'\n", "-proxies", replacement, "-send-addr=false")
	d, err = c.load("")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.proxies) != 1 || d.proxies[0].port != "19003" || !d.proxies[0].config.OmitAddr {
		t.Fatal("CLI override failed")
	}
	c = proxyConfigController(t, "proxies = '"+raw+"'\n", "-proxies", "")
	d, err = c.load("")
	if err != nil || len(d.proxies) != 0 || d.client == nil {
		t.Fatal("empty mappings must leave the proxy idle", err)
	}
}

func TestMultiProxyInvalidConfiguration(t *testing.T) {
	for _, raw := range []string{
		`null`, `{}`, `[{"listen":"127.0.0.1:0","server":"ws://localhost/tunnel"}]`,
		`[{"listen":"127.0.0.1:19001","server":"http://localhost/tunnel"}]`,
		`[{"listen":"127.0.0.1:19001","server":"ws://localhost/tunnel","password":""}]`,
		`[{"listen":"127.0.0.1:19001","server":"ws://localhost/tunnel"},{"listen":"0.0.0.0:19001","server":"ws://localhost/tunnel"}]`,
		`[{"listen":"127.0.0.1:19001","server":"ws://localhost/tunnel"}] {}`,
	} {
		t.Run(raw, func(t *testing.T) {
			c := proxyConfigController(t, "proxies = '"+raw+"'\n")
			if _, err := c.load(""); err == nil {
				t.Fatal("invalid mapping accepted")
			}
		})
	}
}

func proxyConfigController(t *testing.T, content string, extra ...string) *controller {
	t.Helper()
	dir := t.TempDir()
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, []byte(strings.Repeat("39", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "client.cf")
	if err := os.WriteFile(path, []byte("key-file = "+key+"\npassword = multi-test-password\n"+content), 0600); err != nil {
		t.Fatal(err)
	}
	return &controller{ctx: context.Background(), original: appFlags("client"), args: append([]string{"-config", path}, extra...), role: "client", path: path, stats: tunnel.NewStats()}
}
