package admin

import (
	"errors"
	"net/http"
	"testing"
)

func TestVPNControlRequiresAuthAndCSRF(t *testing.T) {
	h, cfg := adminFixture(t)
	cookie, csrf := setupAccount(t, h, cfg)
	var actions []bool
	h.view.ControlVPN = func(start bool) error { actions = append(actions, start); return nil }
	for _, tc := range []struct {
		cookie       *http.Cookie
		csrf, origin string
		want         int
	}{
		{nil, "", "", 401}, {cookie, "", "", 403}, {cookie, csrf, "https://evil.invalid", 403},
	} {
		w := request(h, "POST", "/api/vpn-start", map[string]any{}, tc.cookie, tc.csrf, tc.origin)
		if w.Code != tc.want {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if len(actions) != 0 {
		t.Fatal("unauthorized control executed")
	}
	for _, path := range []string{"/api/vpn-start", "/api/vpn-stop"} {
		w := request(h, "POST", path, map[string]any{}, cookie, csrf, "")
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if len(actions) != 2 || !actions[0] || actions[1] {
		t.Fatal(actions)
	}
	h.view.ControlVPN = func(bool) error { return errors.New("VPN disabled: apply configuration first") }
	if w := request(h, "POST", "/api/vpn-start", nil, cookie, csrf, ""); w.Code != 400 {
		t.Fatal(w.Code)
	}
	h.view.Role = "server"
	if w := request(h, "POST", "/api/vpn-start", nil, cookie, csrf, ""); w.Code != 400 {
		t.Fatal(w.Code)
	}
}

func TestServerVPNControlUsesSameEndpoints(t *testing.T) {
	h, cfg := adminFixture(t)
	h.view.Role = "server"
	cookie, csrf := setupAccount(t, h, cfg)
	var actions []bool
	h.view.ControlVPN = func(start bool) error { actions = append(actions, start); return nil }
	for _, path := range []string{"/api/vpn-start", "/api/vpn-stop"} {
		w := request(h, "POST", path, map[string]any{}, cookie, csrf, "")
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if len(actions) != 2 || !actions[0] || actions[1] {
		t.Fatal(actions)
	}
}
