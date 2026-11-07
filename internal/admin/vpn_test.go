package admin

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"sgate/internal/cli"
	"sgate/internal/vpn"
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

func TestVPNClientFixedAddressHiddenFromLegacyConfig(t *testing.T) {
	fields, err := configFields("vpn-client-ip = 10.77.0.99\nvpn-server = tcp://vpn.example.com:8444/vpn\n")
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range vpnConfigFields(fields, "client") {
		if field.Key == "vpn-client-ip" {
			t.Fatal("legacy fixed address remains editable")
		}
	}
}
func TestVPNFieldsRejectInjectionAndUnknownRoleKeys(t *testing.T) {
	original := []byte("service demo {\n listen = 127.0.0.1:9000\n}\n")
	for _, edits := range []map[string]string{{"vpn-mtu": "12"}, {"vpn-mode": "unexpected"}, {"vpn-dns": "x\nmaster_args = bad"}, {"vpn-quic-listen": "0.0.0.0:8444"}} {
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

func TestVPNOnlyConfigShowsSavedMappingCardsWithoutExposingPasswords(t *testing.T) {
	h, cfg := adminFixture(t)
	config := []byte("vpn-mode = only\nproxies-file = mappings.json\n")
	if err := os.WriteFile(h.view.AppConfigPath, config, 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(h.view.AppConfigPath), "mappings.json")
	mappings := []byte(`[{"listen":"127.0.0.1:2900","server":"quic://example.com:1080/tunnel","addr":"127.0.0.1:2900","username":"test","password":"private-mapping-secret","allow_reverse":true},{"listen":"127.0.0.1:9000","server":"wss://127.0.0.1:8081/tunnel"}]`)
	if err := os.WriteFile(path, mappings, 0600); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := setupAccount(t, h, cfg)
	read := func() map[string]any {
		t.Helper()
		w := request(h, "GET", "/api/config", nil, cookie, csrf, "")
		if w.Code != 200 || strings.Contains(w.Body.String(), "private-mapping-secret") {
			t.Fatal("configuration failed to load safely", w.Code)
		}
		result := object(t, w)
		if result["revision"] != digest(config) {
			t.Fatal("dormant mappings changed the configuration revision")
		}
		for _, item := range result["fields"].([]any) {
			field := item.(map[string]any)
			if field["key"] == "proxies" {
				return field
			}
		}
		t.Fatal("multi-port proxy editor missing")
		return nil
	}
	rows := read()["mappings"].([]any)
	first := rows[0].(map[string]any)
	if len(rows) != 2 || first["listen"] != "127.0.0.1:2900" || first["username"] != "test" || first["allow_reverse"] != true || first["password"] != secretMarker {
		t.Fatal("saved mapping card fields were lost")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if field := read(); field["description"] == "" {
		t.Fatal("missing mapping file reason was not shown")
	}
}

func TestVPNEnableFailureExplainsForeignOptionsAndPreservesConfig(t *testing.T) {
	h, cfg := adminFixture(t)
	h.view.Role = "server"
	old := []byte("service server {\nlisten = 127.0.0.1:8081\nvpn-mode = disabled\nvpn-credentials-file = client.json\n}\n")
	if err := os.WriteFile(h.view.AppConfigPath, old, 0600); err != nil {
		t.Fatal(err)
	}
	h.view.Validate = func(path string) error {
		fs := flag.NewFlagSet("server", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fs.String("listen", "", "")
		vpn.RegisterFlags(fs, "server")
		return cli.Parse(fs, []string{"-config", path})
	}
	cookie, csrf := setupAccount(t, h, cfg)
	view := object(t, request(h, "GET", "/api/config", nil, cookie, "", ""))
	w := request(h, "PUT", "/api/config", map[string]any{"revision": view["revision"], "fields": map[string]string{"vpn-mode": "both"}}, cookie, csrf, "")
	if w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	result := object(t, w)
	reason := result["field_errors"].(map[string]any)["vpn-mode"].(string)
	if !strings.Contains(reason, "vpn-credentials-file") || !strings.Contains(reason, "客户端") {
		t.Fatal(reason)
	}
	actual, _ := os.ReadFile(h.view.AppConfigPath)
	if string(actual) != string(old) {
		t.Fatal("failed validation changed config")
	}
	h.view.Validate = func(string) error {
		return &vpn.ValidationError{Fields: []string{"vpn-mode"}, Message: "独立 VPN 服务端仅支持 Linux，当前系统无法启用。"}
	}
	w = request(h, "PUT", "/api/config", map[string]any{"revision": view["revision"], "fields": map[string]string{"vpn-mode": "both"}}, cookie, csrf, "")
	if w.Code != 400 || !strings.Contains(w.Body.String(), "Linux") {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestLegacyServerVPNConfigCanEnableBothModesFromWeb(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not support VPN server mode")
	}
	for _, mode := range []string{"both", "only"} {
		for _, format := range []string{"plain", "service"} {
			t.Run(mode+"/"+format, func(t *testing.T) {
				h, cfg := adminFixture(t)
				h.view.Role = "server"
				key := filepath.Join(t.TempDir(), "vpn.key")
				if err := os.WriteFile(key, []byte(strings.Repeat("12", 32)), 0600); err != nil {
					t.Fatal(err)
				}
				legacy := "vpn-server = quic://old.invalid:8444/vpn\nvpn-client-ip = 10.77.0.2\nvpn-tls-ca = client.pem\nvpn-routes = invalid-old-route\nvpn-dns = 1.1.1.1\nvpn-dns-service = Wi-Fi\nvpn-block-ipv6 = true\n"
				config := "vpn-mode = disabled\nvpn-quic-listen = ''\nvpn-udp-listen = 127.0.0.1:8445\n" + legacy
				if format == "service" {
					config = "service server {\n" + config + "}\n"
				}
				if err := os.WriteFile(h.view.AppConfigPath, []byte(config), 0600); err != nil {
					t.Fatal(err)
				}
				flags := flag.NewFlagSet("server", flag.ContinueOnError)
				flags.String("key-file", key, "")
				vpn.RegisterFlags(flags, "server")
				h.view.Validate = func(path string) error {
					fs := cli.CloneFlags(flags)
					if err := cli.Parse(fs, []string{"-config", path}); err != nil {
						return err
					}
					_, err := vpn.Prepare("server", fs)
					return err
				}
				cookie, csrf := setupAccount(t, h, cfg)
				view := object(t, request(h, "GET", "/api/config", nil, cookie, "", ""))
				for _, raw := range view["fields"].([]any) {
					if vpn.IsLegacyServerFileOption(raw.(map[string]any)["key"].(string)) {
						t.Fatal("client-only legacy option is editable on the server")
					}
				}
				w := request(h, "PUT", "/api/config", map[string]any{"revision": view["revision"], "fields": map[string]string{"vpn-mode": mode}}, cookie, csrf, "")
				if w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
				saved, err := os.ReadFile(h.view.AppConfigPath)
				if err != nil || !strings.Contains(string(saved), legacy) {
					t.Fatal("legacy lines were modified", err)
				}
				if err := cli.Parse(cli.CloneFlags(flags), []string{"-vpn-server=quic://example.invalid:8444/vpn"}); err == nil {
					t.Fatal("client option accepted as a server command-line flag")
				}
			})
		}
	}
}
