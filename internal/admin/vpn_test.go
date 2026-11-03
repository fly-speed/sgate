package admin

import (
	"os"
	"sgate/internal/vpn"
	"strings"
	"testing"
)

func TestVPNFieldsUpgradeOldConfigThroughAuthenticatedAPI(t *testing.T) {
	h, cfg := adminFixture(t)
	cookie, csrf := setupAccount(t, h, cfg)
	view := object(t, request(h, "GET", "/api/config", nil, cookie, "", ""))
	found := false
	for _, raw := range view["fields"].([]any) {
		f := raw.(map[string]any)
		if f["key"] == "vpn-mode" {
			found = f["value"] == "disabled" && f["group"] == "独立 VPN"
		}
	}
	if !found {
		t.Fatal("old config lacks VPN controls")
	}
	edits := map[string]any{"revision": view["revision"], "fields": map[string]string{"vpn-mode": "disabled", "vpn-mtu": "1200"}}
	w := request(h, "PUT", "/api/config", edits, cookie, csrf, "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	b, _ := os.ReadFile(h.view.AppConfigPath)
	if !strings.Contains(string(b), `vpn-mtu = "1200"`) {
		t.Fatal(string(b))
	}
	fields, e := configFields(string(b))
	if e != nil {
		t.Fatal(e)
	}
	n := 0
	for _, f := range fields {
		if f.Key == "vpn-mtu" {
			n++
		}
	}
	if n != 1 {
		t.Fatal("duplicate key")
	}
	for _, role := range []string{"client", "server"} {
		for _, setting := range vpn.Settings(role) {
			if _, ok := configRules[setting.Key]; !ok {
				t.Fatal("no validation", setting.Key)
			}
		}
	}
}
func TestVPNFieldsRejectInjectionAndUnknownRoleKeys(t *testing.T) {
	original := []byte("service demo {\n listen = 127.0.0.1:9000\n}\n")
	for _, edits := range []map[string]string{{"vpn-mtu": "12"}, {"vpn-mode": "unexpected"}, {"vpn-dns": "x\nmaster_args = bad"}, {"vpn-listen": "0.0.0.0:8444"}} {
		b, err := prepareVPNConfigEdits(original, edits, "client")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = updateConfigFields(b, edits); err == nil {
			t.Fatal("accepted invalid edit", edits)
		}
	}
}

func TestVPNOnlyIgnoresUnusedMappingsAndHonorsCLIOverride(t *testing.T) {
	h, _ := adminFixture(t)
	content := []byte("vpn-mode = only\nproxies-file = /nonexistent/unused.json\n")
	_, path, revision, e := h.mappingSnapshot(content)
	if e != nil || path != "" || revision != digest(content) {
		t.Fatal(path, revision, e)
	}
	// An explicit CLI mode overrides disk settings in the same way as the app.
	h.view.Runtime = func() any { return map[string]any{"cli_overrides": []string{"vpn-mode"}, "vpn_mode": "both"} }
	if h.vpnOnly(content) {
		t.Fatal("disk overrode CLI")
	}
	h.view.Runtime = func() any { return map[string]any{"cli_overrides": []string{"vpn-mode"}, "vpn_mode": "only"} }
	if !h.vpnOnly([]byte("vpn-mode = disabled\n")) {
		t.Fatal("CLI-only mode ignored")
	}
}
