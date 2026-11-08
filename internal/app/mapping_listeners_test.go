package app

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"sgate/internal/aclservice"
)

func TestMappingListenersRespectCLIAndFutureMasterArgs(t *testing.T) {
	disk := `[{"listen":"127.0.0.1:9000","server":"ws://localhost/tunnel"},{"listen":"127.0.0.1:2022","server":"ws://localhost/tunnel"}]`
	override := `[{"listen":"127.0.0.1:9005","server":"ws://localhost/tunnel"}]`
	for _, managed := range []bool{false, true} {
		c := proxyConfigController(t, "proxies = '"+disk+"'\n", "-proxies", override)
		content, err := os.ReadFile(c.path)
		if err != nil {
			t.Fatal(err)
		}
		service := "service client {\nmaster_service = 127.0.0.1|9000\nmaster_args = -proxies " + disk + "\n" + string(content) + "}\n"
		if err = os.WriteFile(c.path, []byte(service), 0600); err != nil {
			t.Fatal(err)
		}
		if managed {
			c.managed = &aclservice.Pool{}
		}
		got, err := c.proxyListeners(c.path)
		want := []string{"127.0.0.1:9005"}
		if managed {
			want = []string{"127.0.0.1:9000", "127.0.0.1:2022"}
		}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatal(managed, got, err)
		}
	}
	c := proxyConfigController(t, "proxies = '"+disk+"'\n", "-proxies", "", "-master_service", "127.0.0.1:9010")
	got, err := c.proxyListeners(c.path)
	if err != nil || !reflect.DeepEqual(got, []string{"127.0.0.1:9010"}) {
		t.Fatal(got, err)
	}
}

func TestMappingListenersDropRemovedFrameworkOverride(t *testing.T) {
	disk := `[{"listen":"127.0.0.1:9000","server":"ws://localhost/tunnel"}]`
	old := `[{"listen":"127.0.0.1:2022","server":"ws://localhost/tunnel"}]`
	c := proxyConfigController(t, "proxies = '"+disk+"'\n", "-proxies", old)
	content, err := os.ReadFile(c.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.path, []byte("service client {\nmaster_args =\n"+string(content)+"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c.managed = &aclservice.Pool{}
	c.controls = map[string]string{"master_args": "-proxies " + old}
	got, err := c.proxyListeners(c.path)
	if err != nil || !reflect.DeepEqual(got, []string{"127.0.0.1:9000"}) {
		t.Fatal(got, err)
	}
}

func TestMappingListenersDoNotExposeInvalidArgumentValues(t *testing.T) {
	c := proxyConfigController(t, testClientMapping(t, "ws://localhost/tunnel")+"")
	content, err := os.ReadFile(c.path)
	if err != nil {
		t.Fatal(err)
	}
	secret := "private-argument-value"
	text := "service client {\nmaster_args = -heartbeat " + secret + "\n" + string(content) + "}\n"
	if err := os.WriteFile(c.path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	c.managed = &aclservice.Pool{}
	_, err = c.proxyListeners(c.path)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatal("invalid argument value was not redacted")
	}
}
