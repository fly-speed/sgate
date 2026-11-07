package admin

import (
	"os"
	"strings"
	"testing"
)

func TestAdministratorPasswordMinimumSixCharacters(t *testing.T) {
	for _, role := range []string{"client", "server"} {
		t.Run(role, func(t *testing.T) {
			h, cfg := adminFixture(t)
			h.view.Role = role
			token, err := os.ReadFile(cfg.SetupTokenFile)
			if err != nil {
				t.Fatal(err)
			}
			setup := map[string]string{"username": "admin", "password": "12345", "setup_token": strings.TrimSpace(string(token))}
			if w := request(h, "POST", "/api/setup", setup, nil, "", ""); w.Code != 400 {
				t.Fatal("five-character setup password accepted", w.Code)
			}
			setup["password"] = "123456"
			w := request(h, "POST", "/api/setup", setup, nil, "", "")
			if w.Code != 200 {
				t.Fatal("six-character setup password rejected", w.Code, w.Body.String())
			}
			cookie := w.Result().Cookies()[0]
			status := object(t, request(h, "GET", "/api/status", nil, cookie, "", ""))
			change := map[string]string{"username": "admin", "current_password": "123456", "password": "abcde"}
			if w := request(h, "PUT", "/api/account", change, cookie, status["csrf"].(string), ""); w.Code != 400 {
				t.Fatal("five-character replacement password accepted", w.Code)
			}
			change["password"] = "abcdef"
			if w := request(h, "PUT", "/api/account", change, cookie, status["csrf"].(string), ""); w.Code != 200 {
				t.Fatal("six-character replacement password rejected", w.Code, w.Body.String())
			}
			if w := request(h, "POST", "/api/login", map[string]string{"username": "admin", "password": "abcdef"}, nil, "", ""); w.Code != 200 {
				t.Fatal("login with six-character password failed", w.Code)
			}
		})
	}
}
