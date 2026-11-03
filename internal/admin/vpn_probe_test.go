package admin

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"

	"sgate/internal/vpn"
)

func TestVPNProbeAPIRequiresAuthValidatesDraftsAndDoesNotSave(t *testing.T) {
	h, cfg := adminFixture(t)
	cookie, csrf := setupAccount(t, h, cfg)
	before, _ := os.ReadFile(h.view.AppConfigPath)
	calls := 0
	h.view.TestVPN = func(_ context.Context, fields map[string]string) (vpn.ProbeResult, error) {
		calls++
		if fields["vpn-server"] != "quic://vpn.example.com:8444/vpn" {
			t.Error(fields)
		}
		return vpn.ProbeResult{OK: true, Stage: "datagram", ElapsedMS: 3}, nil
	}
	input := map[string]any{"fields": map[string]string{"vpn-mode": "both", "vpn-server": "quic://vpn.example.com:8444/vpn"}}
	for _, tc := range []struct {
		cookie       *http.Cookie
		csrf, origin string
		want         int
	}{{nil, "", "", 401}, {cookie, "", "", 403}, {cookie, csrf, "https://evil.invalid", 403}} {
		if w := request(h, "POST", "/api/vpn-test", input, tc.cookie, tc.csrf, tc.origin); w.Code != tc.want {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	for _, fields := range []map[string]string{{"password": "secret"}, {"vpn-listen": ":8444"}, {"vpn-server": "x\npassword=secret"}} {
		if w := request(h, "POST", "/api/vpn-test", map[string]any{"fields": fields}, cookie, csrf, ""); w.Code != 400 {
			t.Fatal(w.Code)
		}
	}
	if calls != 0 {
		t.Fatal("invalid request executed")
	}
	w := request(h, "POST", "/api/vpn-test", input, cookie, csrf, "")
	if w.Code != 200 || object(t, w)["ok"] != true {
		t.Fatal(w.Code, w.Body.String())
	}
	for i := 0; i < cap(h.probeGate); i++ {
		h.probeGate <- struct{}{}
	}
	busy := request(h, "POST", "/api/vpn-test", input, cookie, csrf, "")
	if busy.Code != 429 || calls != 1 {
		t.Fatal("probe gate not enforced", busy.Code, calls)
	}
	for len(h.probeGate) > 0 {
		<-h.probeGate
	}
	after, _ := os.ReadFile(h.view.AppConfigPath)
	if string(after) != string(before) {
		t.Fatal("probe saved config")
	}
	h.view.TestVPN = func(context.Context, map[string]string) (vpn.ProbeResult, error) {
		return vpn.ProbeResult{}, errors.New("secret-private-value")
	}
	w = request(h, "POST", "/api/vpn-test", input, cookie, csrf, "")
	if w.Code != 400 || strings.Contains(w.Body.String(), "secret-private-value") {
		t.Fatal(w.Code, w.Body.String())
	}
	h.view.Role = "server"
	if w = request(h, "POST", "/api/vpn-test", input, cookie, csrf, ""); w.Code != 404 {
		t.Fatal(w.Code)
	}
}
