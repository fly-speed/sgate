package admin

import (
	"os"
	"sgate/internal/vpn"
	"testing"
)

func TestDormantVPNFieldsExposeLiveMetadataForBothRoles(t *testing.T) {
	for _, role := range []string{"client", "server"} {
		t.Run(role, func(t *testing.T) {
			h, cfg := adminFixture(t)
			h.view.Role = role
			h.view.LiveFields = vpn.LiveFields(role)
			h.view.ApplyLive = func([]string) error { return nil }
			cookie, csrf := setupAccount(t, h, cfg)
			view := object(t, request(h, "GET", "/api/config", nil, cookie, "", ""))
			live := map[string]bool{}
			for _, raw := range view["fields"].([]any) {
				field := raw.(map[string]any)
				rule := field["validation"].(map[string]any)
				live[field["key"].(string)] = rule["live"] == true
			}
			for _, key := range vpn.LiveFields(role) {
				if !live[key] {
					t.Fatal("missing live badge for default VPN field", key)
				}
			}
			if live["vpn-mode"] || live["vpn-mtu"] || live["vpn-key-file"] {
				t.Fatal("static VPN setting marked live")
			}
			current, _ := os.ReadFile(h.view.AppConfigPath)
			h.startupRevision = h.restartDigest(current)
			fields := map[string]string{"vpn-noise-rate": "8K"}
			if role == "server" {
				fields["vpn-allow-client-to-client"] = "true"
			}
			response := request(h, "PUT", "/api/config", map[string]any{"revision": view["revision"], "fields": fields}, cookie, csrf, "")
			if response.Code != 200 {
				t.Fatal(response.Code, response.Body.String())
			}
			result := object(t, response)
			if result["requires_restart"] != false || len(result["live_applied"].([]any)) != len(fields) {
				t.Fatal("live save requested a rebuild", result)
			}
		})
	}
}
