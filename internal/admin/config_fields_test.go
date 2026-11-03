package admin

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestConfigFieldsPreserveUneditedBytesAndComments(t *testing.T) {
	original := "\uFEFF# 示例\r\nservice demo {\r\n  # 后端地址\r\n  target = '127.0.0.1:9' # 保留说明\r\n  password = 'a#b'\r\n  master_args = -password hidden\r\n}\r\n"
	fields, err := configFields(redactConfig([]byte(original)))
	if err != nil || len(fields) != 3 {
		t.Fatalf("fields=%v err=%v", fields, err)
	}
	if fields[0].Description != "后端地址" || fields[0].Value != "127.0.0.1:9" || !fields[1].Secret || fields[1].Value != secretMarker || fields[2].Value != secretMarker {
		t.Fatalf("unexpected fields: %+v", fields)
	}
	got, err := updateConfigFields([]byte(original), map[string]string{"target": "127.0.0.1:10", "password": secretMarker})
	want := strings.Replace(original, "'127.0.0.1:9'", `"127.0.0.1:10"`, 1)
	if err != nil || got != want {
		t.Fatalf("modified surrounding data: %q err=%v", got, err)
	}
	got, err = updateConfigFields([]byte(original), map[string]string{})
	if err != nil || got != original {
		t.Fatal("empty update must preserve every byte", err)
	}
}

func TestConfigFieldsRejectInjectionAndPreserveLegacyHash(t *testing.T) {
	content := []byte("password = bare#secret\nlisten = 127.0.0.1:9000\n")
	fields, err := configFields(string(content))
	if err != nil || fields[0].Value != "bare#secret" {
		t.Fatal(fields, err)
	}
	for _, edits := range []map[string]string{{"unknown": "x"}, {"listen": "x\npassword = bad"}, {"listen": "x\r"}, {"listen": "\x00"}} {
		if _, err := updateConfigFields(content, edits); err == nil {
			t.Fatal("accepted invalid edits", edits)
		}
	}
	got, err := updateConfigFields(content, map[string]string{"password": `a"b#c\d`})
	if err != nil {
		t.Fatal(err)
	}
	fields, err = configFields(got)
	if err != nil || fields[0].Value != `a"b#c\d` {
		t.Fatal("secret did not round-trip", err)
	}
}

func TestConfigFieldsSaveApplyAndRejectStaleRevision(t *testing.T) {
	h, cfg := adminFixture(t)
	cookie, csrf := setupAccount(t, h, cfg)
	get := object(t, request(h, "GET", "/api/config", nil, cookie, "", ""))
	if len(get["fields"].([]any)) < 4 || strings.Contains(get["content"].(string), "existing-proxy-secret") {
		t.Fatal("invalid form response")
	}
	applied := 0
	h.view.Apply = func() error { applied++; return nil }
	edits := map[string]any{"fields": map[string]string{"listen": "127.0.0.1:9002"}, "revision": get["revision"], "apply": true}
	w := request(h, "PUT", "/api/config", edits, cookie, csrf, "")
	if w.Code != 200 || applied != 1 {
		t.Fatal(w.Code, w.Body.String(), applied)
	}
	b, _ := os.ReadFile(h.view.AppConfigPath)
	if !strings.Contains(string(b), "password = 'existing-proxy-secret'") || !strings.Contains(string(b), `listen = "127.0.0.1:9002"`) {
		t.Fatal("form edit failed to preserve secret or update listener")
	}
	if w = request(h, "PUT", "/api/config", edits, cookie, csrf, ""); w.Code != 409 || applied != 1 {
		t.Fatal("stale form applied twice", w.Code, applied)
	}
}

func TestConfigFieldsFailedApplyRestoresOriginalFile(t *testing.T) {
	h, cfg := adminFixture(t)
	cookie, csrf := setupAccount(t, h, cfg)
	before, err := os.ReadFile(h.view.AppConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	get := object(t, request(h, "GET", "/api/config", nil, cookie, "", ""))
	h.view.Apply = func() error { return errors.New("test apply failed") }
	w := request(h, "PUT", "/api/config", map[string]any{
		"fields": map[string]string{"listen": "127.0.0.1:9002"}, "revision": get["revision"], "apply": true,
	}, cookie, csrf, "")
	after, err := os.ReadFile(h.view.AppConfigPath)
	if w.Code != 409 || err != nil || string(after) != string(before) {
		t.Fatal("form apply did not roll back", w.Code, err)
	}
	if w = request(h, "GET", "/api/config", nil, cookie, "", ""); w.Code != 200 {
		t.Fatal("login lost after failed apply", w.Code)
	}
}
