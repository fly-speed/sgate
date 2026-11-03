package admin

import (
	"errors"
	"os"
	"testing"
)

func TestNoisePolicyFormSaveAndRollback(t *testing.T) {
	h, cfg := adminFixture(t)
	original := []byte("noise-traffic = true\nnoise-idle = true\nnoise-empty = true\nnoise-idle-after = 1s\n")
	if err := os.WriteFile(h.view.AppConfigPath, original, 0600); err != nil {
		t.Fatal(err)
	}
	h.view.LiveFields = []string{"noise-traffic", "noise-idle", "noise-empty", "noise-idle-after"}
	fieldValue := func(data []byte, key string) string {
		fields, err := configFields(string(data))
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range fields {
			if field.Key == key {
				return field.Value
			}
		}
		return ""
	}
	var applied, restarted int
	fail := false
	h.view.ApplyLive = func(keys []string) error {
		if fail {
			return errors.New("injected online policy failure")
		}
		if len(keys) == 0 {
			t.Error("missing changed live fields")
		}
		data, err := os.ReadFile(h.view.AppConfigPath)
		if err != nil || fieldValue(data, "noise-traffic") != "false" {
			t.Error("online callback ran before persistence")
		}
		applied++
		return nil
	}
	h.view.Apply = func() error { restarted++; return nil }
	h.startupRevision = h.restartDigest(original)
	cookie, csrf := setupAccount(t, h, cfg)
	config := object(t, request(h, "GET", "/api/config", nil, cookie, "", ""))
	for _, raw := range config["fields"].([]any) {
		field := raw.(map[string]any)
		if field["group"] == "独立 VPN" || field["group"] == "服务端连接认证" || field["key"] == "proxies-file" {
			continue
		}
		rule := field["validation"].(map[string]any)
		if rule["live"] != true || rule["hint"] == "" {
			t.Fatal("missing online flag or hint", field)
		}
	}
	response := request(h, "PUT", "/api/config", map[string]any{
		"fields":   map[string]string{"noise-traffic": "false", "noise-empty": "false", "noise-idle-after": "100ms"},
		"revision": config["revision"], "apply": true,
	}, cookie, csrf, "")
	if response.Code != 200 || applied != 1 || restarted != 0 {
		t.Fatal(response.Code, response.Body.String(), applied, restarted)
	}
	saved, _ := os.ReadFile(h.view.AppConfigPath)
	if fieldValue(saved, "noise-idle-after") != "100ms" || h.restartDigest(saved) != h.startupRevision {
		t.Fatal("policy save was not persistent or requires restart")
	}
	for _, bad := range []map[string]string{{"noise-idle-after": "-1s"}, {"noise-idle": "invalid"}} {
		config = object(t, request(h, "GET", "/api/config", nil, cookie, "", ""))
		response = request(h, "PUT", "/api/config", map[string]any{"fields": bad, "revision": config["revision"]}, cookie, csrf, "")
		after, _ := os.ReadFile(h.view.AppConfigPath)
		if response.Code != 400 || string(after) != string(saved) || applied != 1 {
			t.Fatal("invalid policy changed disk or runtime", response.Code)
		}
	}
	fail = true
	config = object(t, request(h, "GET", "/api/config", nil, cookie, "", ""))
	response = request(h, "PUT", "/api/config", map[string]any{"fields": map[string]string{"noise-idle": "false"}, "revision": config["revision"]}, cookie, csrf, "")
	after, _ := os.ReadFile(h.view.AppConfigPath)
	if response.Code != 409 || string(after) != string(saved) || restarted != 0 {
		t.Fatal("failed policy apply did not roll back", response.Code)
	}
}
