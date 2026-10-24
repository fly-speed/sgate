package admin

import (
	"net/http"
	"os"
	"testing"
)

func TestLogoutDeletesOnlyCurrentPersistentSessionAndClearsCookie(t *testing.T) {
	h, cfg := adminFixture(t)
	cookie, csrf := setupAccount(t, h, cfg)
	other := request(h, "POST", "/api/login", map[string]string{
		"username": "admin", "password": "test-management-password",
	}, nil, "", "")
	if other.Code != 200 {
		t.Fatal(other.Code, other.Body.String())
	}
	otherCookie := other.Result().Cookies()[0]
	path := h.sessionPath(cookie.Value)
	if _, err := os.Stat(path); err != nil {
		t.Fatal("login session was not persisted", err)
	}
	w := request(h, "POST", "/api/logout", nil, cookie, csrf, "")
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("logout retained its persistent session", err)
	}
	cleared := w.Result().Cookies()
	if len(cleared) != 1 || cleared[0].Name != cookie.Name || cleared[0].Value != "" || cleared[0].MaxAge >= 0 || !cleared[0].HttpOnly {
		t.Fatal("logout did not expire the login cookie", cleared)
	}
	restarted, err := New(cfg, h.view)
	if err != nil {
		t.Fatal(err)
	}
	if w = request(restarted, "GET", "/api/config", nil, cookie, "", ""); w.Code != http.StatusUnauthorized {
		t.Fatal("revoked cookie became usable after restart", w.Code)
	}
	if w = request(restarted, "GET", "/api/config", nil, otherCookie, "", ""); w.Code != http.StatusOK {
		t.Fatal("logout removed another browser's session", w.Code)
	}
}
