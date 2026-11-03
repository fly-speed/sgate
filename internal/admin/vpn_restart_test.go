package admin

import (
	"os"
	"testing"
)

func TestVPNRestartRequiresCSRFAndRevisionAndKeepsProxyPending(t *testing.T) {
	for _, role := range []string{"client", "server"} {
		t.Run(role, func(t *testing.T) {
			h, cfg := adminFixture(t)
			h.view.Role = role
			calls := 0
			h.view.RestartVPN = func() error { calls++; return nil }
			cookie, csrf := setupAccount(t, h, cfg)
			original := []byte("heartbeat = 20s\nvpn-mode = both\nvpn-mtu = 1280\n")
			current := []byte("heartbeat = 30s\nvpn-mode = both\nvpn-mtu = 1200\n")
			h.startupContent = original
			h.startupRevision = h.restartDigest(original)
			if err := os.WriteFile(h.view.AppConfigPath, current, 0600); err != nil {
				t.Fatal(err)
			}
			revision := h.configRevision(current)
			if w := request(h, "POST", "/api/vpn-restart", map[string]string{"revision": revision}, cookie, "", ""); w.Code != 403 {
				t.Fatal(w.Code)
			}
			if w := request(h, "POST", "/api/vpn-restart", map[string]string{"revision": "stale"}, cookie, csrf, ""); w.Code != 409 {
				t.Fatal(w.Code)
			}
			if calls != 0 {
				t.Fatal("invalid restart request reached controller")
			}
			w := request(h, "POST", "/api/vpn-restart", map[string]string{"revision": revision}, cookie, csrf, "")
			if w.Code != 200 || calls != 1 {
				t.Fatal(w.Code, w.Body.String())
			}
			result := object(t, w)
			if result["requires_restart"] != true {
				t.Fatal("pending proxy setting lost")
			}
			fields, err := configFields(string(h.startupContent))
			if err != nil {
				t.Fatal(err)
			}
			values := map[string]string{}
			for _, f := range fields {
				values[f.Key] = f.Value
			}
			if values["heartbeat"] != "20s" || values["vpn-mtu"] != "1200" {
				t.Fatal("VPN restart applied proxy config or missed VPN config", values)
			}
			content, _ := os.ReadFile(h.view.AppConfigPath)
			if string(content) != string(current) {
				t.Fatal("restart endpoint rewrote configuration")
			}
		})
	}
}
