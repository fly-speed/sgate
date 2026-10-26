package admin

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestSharedConfigDoesNotRetainManagementAddress(t *testing.T) {
	for _, merged := range []bool{false, true} {
		for _, address := range []string{"", "127.0.0.1:9082", "retired-address"} {
			t.Run(fmt.Sprintf("merged=%t/address=%s", merged, address), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "admin.cf")
				text := ""
				if address != "" {
					text = "listen = " + address + "\n"
				}
				if merged {
					text = "service server {\nadmin_" + text + "}\n"
					if address == "" {
						text = "service server {\nadmin_enabled = true\n}\n"
					}
				}
				if err := os.WriteFile(path, []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
				cfg, err := LoadSharedConfig(path)
				if err != nil || cfg.Listen != "" {
					t.Fatalf("shared management retained an address: %q, %v", cfg.Listen, err)
				}
			})
		}
	}
}

func TestSharedCookieMigrationPreservesExpiryAndLogout(t *testing.T) {
	for _, scheme := range []string{"http", "https"} {
		t.Run(scheme, func(t *testing.T) {
			legacy, cfg := adminFixture(t)
			legacy.view.Role = "server"
			old, csrf := setupAccount(t, legacy, cfg)
			original, err := os.ReadFile(legacy.sessionPath(old.Value))
			if err != nil {
				t.Fatal(err)
			}
			cfg.shared, cfg.Listen = true, ""
			current, err := New(cfg, legacy.view)
			if err != nil {
				t.Fatal(err)
			}
			url := scheme + "://example.com/api/stats"
			response := request(current, "GET", url, nil, old, "", "")
			if response.Code != http.StatusOK {
				t.Fatal("old login did not survive removal of the port", response.Code)
			}
			var migrated *http.Cookie
			var retired bool
			for _, cookie := range response.Result().Cookies() {
				if cookie.Name == current.cookie {
					migrated = cookie
				}
				retired = retired || (cookie.Name == old.Name && cookie.MaxAge < 0)
			}
			if migrated == nil || !retired || migrated.Value != old.Value || !migrated.Expires.Equal(old.Expires) ||
				migrated.Secure != (scheme == "https") || !migrated.HttpOnly || migrated.SameSite != http.SameSiteStrictMode {
				t.Fatal("migration changed session lifetime or cookie security attributes")
			}
			after, err := os.ReadFile(current.sessionPath(old.Value))
			if err != nil || !bytes.Equal(original, after) {
				t.Fatal("cookie migration rewrote persistent session data")
			}
			cfg.Listen = "ignored-address"
			restarted, err := New(cfg, legacy.view)
			if err != nil || restarted.cookie != current.cookie {
				t.Fatal("server login identity depends on a listener", err)
			}
			if w := request(restarted, "GET", url, nil, migrated, "", ""); w.Code != http.StatusOK {
				t.Fatal("migrated session failed after restart", w.Code)
			}
			if w := request(restarted, "POST", "/api/logout", nil, old, csrf, ""); w.Code != http.StatusOK {
				t.Fatal("logout using an old cookie failed", w.Code)
			}
			if _, err := os.Stat(current.sessionPath(old.Value)); !os.IsNotExist(err) {
				t.Fatal("logout retained the persistent session", err)
			}
			for _, cookie := range []*http.Cookie{old, migrated} {
				if w := request(restarted, "GET", url, nil, cookie, "", ""); w.Code != http.StatusUnauthorized {
					t.Fatal("revoked session remained usable", w.Code)
				}
			}
		})
	}
}

func TestSharedManagementHidesAndRejectsLegacyPort(t *testing.T) {
	old, cfg := adminFixture(t)
	old.view.Role = "server"
	if err := os.WriteFile(old.view.AppConfigPath, []byte("service server {\nlisten = 127.0.0.1:8081\nadmin_listen = 127.0.0.1:9082\n}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.shared, cfg.Listen = true, ""
	h, err := New(cfg, old.view)
	if err != nil {
		t.Fatal(err)
	}
	cookie, csrf := setupAccount(t, h, cfg)
	config := object(t, request(h, "GET", "/api/config", nil, cookie, "", ""))
	for _, raw := range config["fields"].([]any) {
		if raw.(map[string]any)["key"] == "admin_listen" {
			t.Fatal("server form still exposes a management port")
		}
	}
	before, _ := os.ReadFile(h.view.AppConfigPath)
	w := request(h, "PUT", "/api/config", map[string]any{
		"fields": map[string]string{"admin_listen": "127.0.0.1:9999"}, "revision": config["revision"],
	}, cookie, csrf, "")
	after, _ := os.ReadFile(h.view.AppConfigPath)
	if w.Code != http.StatusBadRequest || !bytes.Equal(before, after) {
		t.Fatal("server allowed editing the retired management port", w.Code)
	}
}

func TestSharedManagementRejectsForeignLegacySession(t *testing.T) {
	other, otherConfig := adminFixture(t)
	cookie, _ := setupAccount(t, other, otherConfig)
	h, cfg := adminFixture(t)
	cfg.shared, cfg.Listen = true, ""
	shared, err := New(cfg, h.view)
	if err != nil {
		t.Fatal(err)
	}
	response := request(shared, "GET", "/api/stats", nil, cookie, "", "")
	if response.Code != http.StatusUnauthorized || len(response.Result().Cookies()) != 0 {
		t.Fatal("foreign cookie was migrated", response.Code)
	}
}
