package admin

import (
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestClientManagementAliases(t *testing.T) {
	h, c := adminFixture(t)
	redirect := request(h, "GET", "/admin?from=bookmark", nil, nil, "", "")
	if redirect.Code != http.StatusPermanentRedirect || redirect.Header().Get("Location") != "/admin/?from=bookmark" {
		t.Fatal(redirect.Code, redirect.Header())
	}
	for _, path := range []string{"/", "/assets/app.css", "/assets/app.js"} {
		root := request(h, "GET", path, nil, nil, "", "")
		alias := request(h, "GET", "/admin"+path, nil, nil, "", "")
		if root.Code != 200 || alias.Code != 200 || root.Body.String() != alias.Body.String() || root.Header().Get("Content-Type") != alias.Header().Get("Content-Type") {
			t.Fatalf("resource mismatch %s: %d/%d", path, root.Code, alias.Code)
		}
	}
	for _, path := range []string{"/admin/api/config", "/admin/api/stats"} {
		if w := request(h, "GET", path, nil, nil, "", ""); w.Code != 401 {
			t.Fatal(path, w.Code)
		}
	}
	_, _ = setupAccount(t, h, c)
	login := request(h, "POST", "/admin/api/login", map[string]string{"username": "admin", "password": "test-management-password"}, nil, "", "")
	if login.Code != 200 {
		t.Fatal(login.Code, login.Body.String())
	}
	cookie := login.Result().Cookies()[0]
	if cookie.Path != "/" {
		t.Fatal("aliases must share the session cookie", cookie.Path)
	}
	csrf := object(t, login)["csrf"].(string)
	config := object(t, request(h, "GET", "/admin/api/config", nil, cookie, "", ""))
	calls := 0
	h.view.Apply = func() error { calls++; return nil }
	body := map[string]any{"content": config["content"].(string) + "\n# alias save\n", "revision": config["revision"], "apply": true}
	if w := request(h, "PUT", "/admin/api/config", body, cookie, csrf, "http://attacker.example"); w.Code != 403 {
		t.Fatal("cross-origin save allowed", w.Code)
	}
	if w := request(h, "PUT", "/admin/api/config", body, cookie, "", ""); w.Code != 403 {
		t.Fatal("CSRF check bypassed", w.Code)
	}
	if w := request(h, "PUT", "/admin/api/config", body, cookie, csrf, ""); w.Code != 200 || calls != 1 {
		t.Fatal(w.Code, w.Body.String(), calls)
	}
	data, err := os.ReadFile(h.view.AppConfigPath)
	if err != nil || !strings.Contains(string(data), "# alias save") {
		t.Fatal("save failed", err)
	}
	if w := request(h, "POST", "/admin/api/apply", map[string]any{}, cookie, csrf, ""); w.Code != 200 || calls != 2 {
		t.Fatal(w.Code, w.Body.String(), calls)
	}
	for _, path := range []string{"/api/stats", "/admin/api/stats"} {
		if w := request(h, "GET", path, nil, cookie, "", ""); w.Code != 200 {
			t.Fatal(path, w.Code)
		}
	}
	if w := request(h, "POST", "/admin/api/logout", map[string]any{}, cookie, csrf, ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request(h, "GET", "/api/stats", nil, cookie, "", ""); w.Code != 401 {
		t.Fatal("logout did not revoke shared session", w.Code)
	}
	for _, path := range []string{"/administrator/", "/admin/admin/", "/admin/missing"} {
		if w := request(h, "GET", path, nil, nil, "", ""); w.Code != 404 {
			t.Fatal(path, w.Code)
		}
	}
}

func TestServerManagementDoesNotAddClientAliases(t *testing.T) {
	h, _ := adminFixture(t)
	h.view.Role = "server"
	for _, path := range []string{"/admin", "/admin/", "/admin/api/status"} {
		if w := request(h, "GET", path, nil, nil, "", ""); w.Code != 404 {
			t.Fatal(path, w.Code)
		}
	}
}
