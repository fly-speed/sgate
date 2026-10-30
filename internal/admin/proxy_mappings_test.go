package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sgate/internal/serviceconfig"
	"strconv"
	"strings"
	"testing"
)

func TestProxyMappingFormSaveAndRollback(t *testing.T) {
	h, cfg := adminFixture(t)
	cookie, csrf := setupAccount(t, h, cfg)
	get := func() map[string]any { return object(t, request(h, "GET", "/admin/api/config", nil, cookie, "", "")) }
	save := func(mappings any, apply bool, revision any) int {
		t.Helper()
		w := request(h, "PUT", "/admin/api/config", map[string]any{"fields": map[string]string{}, "proxies": mappings, "revision": revision, "apply": apply}, cookie, csrf, "")
		return w.Code
	}
	before := get()
	mappings := []proxyMapping{{Listen: "127.0.0.1:9001", Server: "ws://localhost:8081/tunnel?keep=1", Addr: "localhost:8080"}}
	if code := save(mappings, false, before["revision"]); code != 200 {
		t.Fatal(code)
	}
	after := get()
	fields := after["fields"].([]any)
	var proxy map[string]any
	for _, raw := range fields {
		field := raw.(map[string]any)
		if field["key"] == "proxies" {
			proxy = field
			break
		}
	}
	if proxy["kind"] != "proxies" || proxy["value"] != "" || len(proxy["mappings"].([]any)) != 1 {
		t.Fatal(proxy)
	}
	if code := save(mappings, false, before["revision"]); code != 409 {
		t.Fatal("stale revision accepted", code)
	}
	original, _ := os.ReadFile(h.view.AppConfigPath)
	if !strings.Contains(string(original), "existing-proxy-secret") {
		t.Fatal("secret lost")
	}
	applied := 0
	h.view.Apply = func() error { applied++; return nil }
	mappings[0].Addr = "localhost:8088"
	if code := save(mappings, true, after["revision"]); code != 200 || applied != 1 {
		t.Fatal(code, applied)
	}
	after = get()
	original, _ = os.ReadFile(h.view.AppConfigPath)
	h.view.Apply = func() error { return errors.New("apply failed") }
	mappings[0].Addr = "localhost:8089"
	if code := save(mappings, true, after["revision"]); code != 409 {
		t.Fatal(code)
	}
	restored, _ := os.ReadFile(h.view.AppConfigPath)
	if string(restored) != string(original) {
		t.Fatal("rollback failed")
	}
	if code := save([]proxyMapping{}, false, get()["revision"]); code != 200 {
		t.Fatal("disable failed", code)
	}
}

func TestProxyMappingsQuotedServiceRoundTrip(t *testing.T) {
	original := []byte("service demo {\n # old comment\n password = 'a#b'\n proxies = '[{\"listen\":\"127.0.0.1:9001\",\"server\":\"ws://localhost/tunnel?keep=1&addr=localhost:80\"}]' # keep\n}\n")
	fields, err := configFields(string(original))
	if err != nil {
		t.Fatal(err)
	}
	mappings, err := proxyMappings(fields[1].Value)
	if err != nil || mappings[0].Addr != "localhost:80" || mappings[0].Server != "ws://localhost/tunnel?keep=1" {
		t.Fatal(mappings, err)
	}
	mappings[0].Addr = "localhost:81"
	content, err := updateMappingFields(original, map[string]string{}, mappings)
	if err != nil || !strings.Contains(content, "# keep") || !strings.Contains(content, "password = 'a#b'") {
		t.Fatal(content, err)
	}
	fields, err = configFields(content)
	if err != nil {
		t.Fatal(err)
	}
	mappings, err = proxyMappings(fields[1].Value)
	if err != nil || mappings[0].Addr != "localhost:81" {
		t.Fatal(mappings, err)
	}
	added, err := updateMappingFields([]byte("service demo {\n server = ws://localhost/tunnel\n}\n"), nil, mappings)
	if err != nil || !strings.Contains(added, "proxies =") || !strings.HasSuffix(added, "}\n") {
		t.Fatal(added, err)
	}
}

func TestProxyMappingValidation(t *testing.T) {
	for _, mappings := range [][]proxyMapping{
		{{Listen: "127.0.0.1:0", Server: "ws://localhost/tunnel"}},
		{{Listen: "127.0.0.1:9001", Server: "http://localhost/tunnel"}},
		{{Listen: "127.0.0.1:9001", Server: "ws://localhost/tunnel", Addr: "bad"}},
		{{Listen: "127.0.0.1:9001", Server: "ws://localhost/tunnel"}, {Listen: "0.0.0.0:9001", Server: "ws://localhost/tunnel"}},
	} {
		if _, err := encodeProxyMappings(mappings); err == nil {
			t.Fatal("invalid mapping accepted", mappings)
		}
	}
}

// 原单引号 JSON 可以加载，表单保存若改成双引号转义，可能超过模板 4095 字节限制。
func TestProxyMappingSaveDoesNotExpandQuotedJSON(t *testing.T) {
	var mappings []proxyMapping
	var raw string
	for n := 1; n <= 64; n++ {
		mappings = append(mappings, proxyMapping{Listen: fmt.Sprintf("127.0.0.1:%d", 9000+n), Server: "ws://localhost:8081/tunnel?keep=" + strings.Repeat("x", 40)})
		data, _ := json.Marshal(mappings)
		raw = string(data)
		if len("proxies = "+strconv.Quote(raw)) > 4095 {
			break
		}
	}
	if len("proxies = '"+raw+"'") > 4095 {
		t.Fatal("fixture exceeds original line limit")
	}
	content, err := updateConfigFields([]byte("service demo {\n proxies =\n}\n"), map[string]string{"proxies": raw})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "client.cf")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	values, merged, err := serviceconfig.Read(path)
	if err != nil || !merged || values["proxies"] != raw {
		t.Fatal("saved mappings no longer load", err)
	}
}

func TestProxyMappingExplicitTargetCanBeCleared(t *testing.T) {
	raw := `[{"listen":"127.0.0.1:9001","server":"ws://localhost/tunnel?keep=1&addr=old.example:80&addr=old.example:81","addr":"chosen.example:443"}]`
	mappings, err := proxyMappings(raw)
	if err != nil || len(mappings) != 1 {
		t.Fatal(mappings, err)
	}
	if mappings[0].Addr != "chosen.example:443" || mappings[0].Server != "ws://localhost/tunnel?keep=1" {
		t.Fatal("URL target still shadows the structured editor", mappings)
	}
	mappings[0].Addr = ""
	saved, err := encodeProxyMappings(mappings)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(saved, "old.example") || strings.Contains(saved, "chosen.example") {
		t.Fatal("cleared backend returned", saved)
	}
	restored, err := proxyMappings(saved)
	if err != nil || restored[0].Addr != "" || restored[0].Server != "ws://localhost/tunnel?keep=1" {
		t.Fatal(restored, err)
	}
}
