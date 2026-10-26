package admin

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"sgate/internal/tunnel"
)

func TestProxyConnectivityAPIAuthValidationAndNoSave(t *testing.T) {
	h, cfg := adminFixture(t)
	cookie, csrf := setupAccount(t, h, cfg)
	before, _ := os.ReadFile(h.view.AppConfigPath)
	var calls atomic.Int32
	h.view.TestProxy = func(ctx context.Context, m ProxyMapping) (tunnel.ProbeResult, error) {
		calls.Add(1)
		if m.Listen != "127.0.0.1:9002" || m.Addr != "localhost:8088" {
			t.Error("wrong draft", m)
		}
		return tunnel.ProbeResult{OK: true, Stage: "backend", Detail: "reachable", ElapsedMS: 2}, nil
	}
	mapping := ProxyMapping{Listen: "127.0.0.1:9002", Server: "ws://localhost:8081/tunnel", Addr: "localhost:8088"}
	for _, tc := range []struct {
		cookie       *http.Cookie
		csrf, origin string
		want         int
	}{
		{nil, "", "", 401}, {cookie, "", "", 403}, {cookie, csrf, "https://evil.invalid", 403},
	} {
		w := request(h, "POST", "/api/proxy-test", mapping, tc.cookie, tc.csrf, tc.origin)
		if w.Code != tc.want {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	bad := mapping
	bad.Server = "http://invalid"
	if w := request(h, "POST", "/api/proxy-test", bad, cookie, csrf, ""); w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatal("invalid/unauthenticated probe executed")
	}
	w := request(h, "POST", "/admin/api/proxy-test", mapping, cookie, csrf, "")
	if w.Code != 200 || object(t, w)["ok"] != true {
		t.Fatal(w.Code, w.Body.String())
	}
	after, _ := os.ReadFile(h.view.AppConfigPath)
	if string(before) != string(after) {
		t.Fatal("testing wrote configuration")
	}
	h.view.TestProxy = func(context.Context, ProxyMapping) (tunnel.ProbeResult, error) {
		return tunnel.ProbeResult{}, errors.New("secret-password")
	}
	w = request(h, "POST", "/api/proxy-test", mapping, cookie, csrf, "")
	if w.Code != 400 || strings.Contains(w.Body.String(), "secret-password") {
		t.Fatal("config error leaked", w.Body.String())
	}
	h.view.Role = "server"
	if w := request(h, "POST", "/api/proxy-test", mapping, cookie, csrf, ""); w.Code != 404 {
		t.Fatal(w.Code)
	}
}

func TestProxyConnectivityAPIBoundedConcurrency(t *testing.T) {
	h, cfg := adminFixture(t)
	cookie, csrf := setupAccount(t, h, cfg)
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	h.view.TestProxy = func(ctx context.Context, _ ProxyMapping) (tunnel.ProbeResult, error) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return tunnel.ProbeResult{OK: true}, nil
	}
	mapping := ProxyMapping{Listen: "127.0.0.1:9002", Server: "ws://localhost:8081/tunnel"}
	done := make(chan int, 4)
	for i := 0; i < 4; i++ {
		go func() { done <- request(h, "POST", "/api/proxy-test", mapping, cookie, csrf, "").Code }()
	}
	defer close(release)
	for i := 0; i < 4; i++ {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("probe did not enter")
		}
	}
	if w := request(h, "POST", "/api/proxy-test", mapping, cookie, csrf, ""); w.Code != 429 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request(h, "GET", "/api/stats", nil, cookie, "", ""); w.Code != 200 {
		t.Fatal("probes blocked statistics", w.Code)
	}
	// Release without racing close against the deferred close.
	for i := 0; i < 4; i++ {
		release <- struct{}{}
	}
	for i := 0; i < 4; i++ {
		select {
		case status := <-done:
			if status != 200 {
				t.Fatal(status)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("probe did not finish")
		}
	}
	if len(h.probeGate) != 0 {
		t.Fatal("probe permits leaked")
	}
}
