package admin

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestClientCredentialsOnOldConfiguration(t *testing.T) {
	for _, merged := range []bool{false, true} {
		name := "plain"
		if merged {
			name = "merged"
		}
		t.Run(name, func(t *testing.T) {
			h, cfg := adminFixture(t)
			original := "listen = 127.0.0.1:9000\n"
			if merged {
				original = "service client {\n" + original + "}\n"
			}
			if err := os.WriteFile(h.view.AppConfigPath, []byte(original), 0600); err != nil {
				t.Fatal(err)
			}
			cookie, csrf := setupAccount(t, h, cfg)
			get := object(t, request(h, "GET", "/api/config", nil, cookie, "", ""))
			found := map[string]bool{}
			for _, item := range get["fields"].([]any) {
				field := item.(map[string]any)
				if field["group"] == "服务端连接认证" {
					found[field["key"].(string)] = true
					if field["key"] == "password" && field["secret"] != true {
						t.Fatal("password is not masked")
					}
				}
			}
			if !found["username"] || !found["password"] {
				t.Fatal("old configuration did not expose credentials")
			}
			applied := 0
			h.view.Apply = func() error { applied++; return nil }
			payload := map[string]any{"revision": get["revision"], "fields": map[string]string{"username": "alice", "password": "alice-secret"}, "apply": true}
			w := request(h, "PUT", "/api/config", payload, cookie, csrf, "")
			if w.Code != 200 || applied != 1 {
				t.Fatal(w.Code, w.Body.String(), applied)
			}
			saved, _ := os.ReadFile(h.view.AppConfigPath)
			if !strings.Contains(string(saved), `username = "alice"`) || !strings.Contains(string(saved), `password = "alice-secret"`) {
				t.Fatal("credentials not persisted")
			}
			if merged && strings.Index(string(saved), "username =") > strings.Index(string(saved), "}") {
				t.Fatal("credential inserted outside service block")
			}
			get = object(t, request(h, "GET", "/api/config", nil, cookie, "", ""))
			for _, field := range get["fields"].([]any) {
				if field.(map[string]any)["value"] == "alice-secret" {
					t.Fatal("password leaked")
				}
			}
			// Username-only saves preserve the password; failed application rolls back both.
			payload = map[string]any{"revision": get["revision"], "fields": map[string]string{"username": "bob"}, "apply": true}
			h.view.Apply = func() error { return errors.New("apply failed") }
			w = request(h, "PUT", "/api/config", payload, cookie, csrf, "")
			after, _ := os.ReadFile(h.view.AppConfigPath)
			if w.Code != 409 || string(after) != string(saved) {
				t.Fatal("failed credential apply did not roll back", w.Code)
			}
			h.view.Apply = func() error { return nil }
			w = request(h, "PUT", "/api/config", payload, cookie, csrf, "")
			after, _ = os.ReadFile(h.view.AppConfigPath)
			if w.Code != 200 || !strings.Contains(string(after), `password = "alice-secret"`) {
				t.Fatal("username-only save lost password", w.Code)
			}
		})
	}
}

func TestTunnelAccountConfigurationRules(t *testing.T) {
	for _, username := range []string{"", "alice", "客户A"} {
		if err := validateConfigEdit("username", username); err != nil {
			t.Fatal(err)
		}
	}
	for _, username := range []string{"a b", "alice\n", strings.Repeat("界", 43)} {
		if err := validateConfigEdit("username", username); err == nil {
			t.Fatal("invalid username accepted")
		}
	}
	if _, ok := argumentOptions("server")["accounts-file"]; !ok {
		t.Fatal("server account file not editable")
	}
	if _, ok := argumentOptions("client")["username"]; !ok {
		t.Fatal("client username not editable")
	}
	if _, ok := argumentOptions("server")["username"]; ok {
		t.Fatal("client username leaked to server options")
	}
	if _, ok := argumentOptions("client")["accounts-file"]; ok {
		t.Fatal("server account file leaked to client options")
	}
}
