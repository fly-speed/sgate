package admin

import (
	"os"
	"testing"
)

func TestClientConfigOnlyExposesExplicitMappings(t *testing.T) {
	h, cfg := adminFixture(t)
	cookie, csrf := setupAccount(t, h, cfg)
	w := request(h, "GET", "/api/config", nil, cookie, "", "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	view := object(t, w)
	found := false
	for _, entry := range view["fields"].([]any) {
		key := entry.(map[string]any)["key"]
		if key == "server" {
			t.Fatal("removed server field exposed")
		}
		if key == "proxies" {
			found = true
		}
	}
	if !found {
		t.Fatal("idle client cannot add its first mapping")
	}
	before, _ := os.ReadFile(h.view.AppConfigPath)
	for _, endpoint := range []string{"", "ws://localhost/tunnel"} {
		w = request(h, "PUT", "/api/config", map[string]any{"revision": view["revision"], "fields": map[string]string{"server": endpoint}, "apply": true}, cookie, csrf, "")
		if w.Code != 400 {
			t.Fatal("removed server field accepted", w.Code, w.Body.String())
		}
	}
	after, _ := os.ReadFile(h.view.AppConfigPath)
	if string(after) != string(before) {
		t.Fatal("rejected option changed configuration")
	}
	if _, ok := argumentOptions("client")["server"]; ok {
		t.Fatal("removed CLI option exposed")
	}
}
