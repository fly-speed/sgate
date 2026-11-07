package admin

import (
	"errors"
	"os"
	"strings"
	"testing"

	"sgate/internal/serviceconfig"
)

func TestMappingListenersSavedTogetherAndRolledBack(t *testing.T) {
	h, cfg := adminFixture(t)
	original := "service client {\n # keep listener comment\n master_service = 127.0.0.1:9000 # keep inline\n password = 'existing-proxy-secret'\n proxies =\n}\n"
	if err := os.WriteFile(h.view.AppConfigPath, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := setupAccount(t, h, cfg)
	mapping := func(address string) proxyMapping {
		return proxyMapping{Listen: address, Server: "ws://localhost/tunnel"}
	}
	save := func(routes []proxyMapping, fields map[string]string, apply bool, status int) map[string]any {
		t.Helper()
		get := object(t, request(h, "GET", "/api/config", nil, cookie, "", ""))
		response := request(h, "PUT", "/api/config", map[string]any{"proxies": routes, "fields": fields, "revision": get["revision"], "apply": apply}, cookie, csrf, "")
		if response.Code != status {
			t.Fatalf("save: %d %s", response.Code, response.Body.String())
		}
		if status != 200 {
			return nil
		}
		return object(t, response)
	}
	check := func(want string, count int) {
		t.Helper()
		values, _, err := serviceconfig.Read(h.view.AppConfigPath)
		if err != nil {
			t.Fatal(err)
		}
		if values["master_service"] != want {
			t.Fatalf("listeners %q, want %q", values["master_service"], want)
		}
		mappings, err := proxyMappings(values["proxies"])
		if err != nil || len(mappings) != count {
			t.Fatal(mappings, err)
		}
		raw, _ := os.ReadFile(h.view.AppConfigPath)
		for _, text := range []string{"# keep listener comment", "# keep inline", "password = 'existing-proxy-secret'"} {
			if !strings.Contains(string(raw), text) {
				t.Fatal("lost comment or secret")
			}
		}
	}
	routes := []proxyMapping{mapping("127.0.0.1:9000"), mapping("127.0.0.1:2022")}
	result := save(routes, nil, false, 200)
	if result["synced_fields"].(map[string]any)["master_service"] != "127.0.0.1|9000, 127.0.0.1|2022" {
		t.Fatal(result)
	}
	check("127.0.0.1|9000, 127.0.0.1|2022", 2)
	save(routes[1:], nil, false, 200)
	check("127.0.0.1|2022", 1)
	save([]proxyMapping{mapping("[::1]:2022")}, nil, false, 200)
	check("[::1]|2022", 1)
	save([]proxyMapping{}, map[string]string{"master_service": "127.0.0.1|9003"}, false, 200)
	check("127.0.0.1|9003", 0)
	before, _ := os.ReadFile(h.view.AppConfigPath)
	save(routes, map[string]string{"master_service": "127.0.0.1|9009"}, false, 400)
	after, _ := os.ReadFile(h.view.AppConfigPath)
	if string(after) != string(before) {
		t.Fatal("conflict changed file")
	}
	h.view.Apply = func() error { return errors.New("framework restart required") }
	save(routes, nil, true, 409)
	after, _ = os.ReadFile(h.view.AppConfigPath)
	if string(after) != string(before) {
		t.Fatal("apply failure did not restore both settings")
	}
	h.view.Validate = func(string) error { return errors.New("validation failed") }
	save(routes, nil, false, 400)
	after, _ = os.ReadFile(h.view.AppConfigPath)
	if string(after) != string(before) {
		t.Fatal("validation failure changed file")
	}
	// Saving retains the current login rather than recreating/revoking sessions.
	status := object(t, request(h, "GET", "/api/status", nil, cookie, "", ""))
	if status["authenticated"] != true {
		t.Fatal("session was lost")
	}
}
