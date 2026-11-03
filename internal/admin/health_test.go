package admin

import "testing"

func TestAuthenticatedTransportHealthEndpoint(t *testing.T) {
	h, cfg := adminFixture(t)
	cookie, _ := setupAccount(t, h, cfg)
	if got := request(h, "GET", "/api/health", nil, nil, "", ""); got.Code != 401 {
		t.Fatal("health exposed without authentication", got.Code)
	}
	healthy := false
	h.view.Runtime = func() any {
		return map[string]any{"healthy": healthy, "running": true, "quic": map[string]any{"enabled": true, "healthy": healthy, "state": "recovering"}, "private_configuration": "must not be exposed"}
	}
	got := request(h, "GET", "/api/health", nil, cookie, "", "")
	if got.Code != 503 || object(t, got)["healthy"] != false {
		t.Fatal("failed UDP listener reported healthy", got.Body.String())
	}
	if _, found := object(t, got)["private_configuration"]; found {
		t.Fatal("health leaked unrelated runtime fields")
	}
	stats := request(h, "GET", "/api/stats", nil, cookie, "", "")
	if stats.Code != 200 || object(t, stats)["health"].(map[string]any)["healthy"] != false {
		t.Fatal("polling stats hid degraded health")
	}
	healthy = true
	got = request(h, "GET", "/api/health", nil, cookie, "", "")
	if got.Code != 200 || object(t, got)["healthy"] != true {
		t.Fatal("recovered transport stayed unhealthy")
	}
}
