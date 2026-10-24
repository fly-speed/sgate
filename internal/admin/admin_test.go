package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sgate/internal/tunnel"
)

func adminFixture(t *testing.T) (*Handler, Config) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "application.conf")
	if e := os.WriteFile(path, []byte("\uFEFFpassword = 'existing-proxy-secret'\nlisten = 127.0.0.1:9000\n"), 0600); e != nil {
		t.Fatal(e)
	}
	webDir, e := filepath.Abs("../../web/admin")
	if e != nil {
		t.Fatal(e)
	}
	cfg := Config{WebDir: webDir, Listen: "127.0.0.1:9081", UsersFile: filepath.Join(dir, "users.json"), SessionDir: filepath.Join(dir, "sessions"), SetupTokenFile: filepath.Join(dir, "setup.token"), SessionTimeout: time.Hour, MaxSessions: 10}
	h, e := New(cfg, View{Role: "client", AppConfigPath: path, Stats: tunnel.NewStats(), Validate: func(path string) error {
		b, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		if strings.Contains(string(b), "invalid") {
			return errors.New("invalid configuration")
		}
		return nil
	}, Apply: func() error { return nil }})
	if e != nil {
		t.Fatal(e)
	}
	return h, cfg
}
func request(h *Handler, method, path string, body any, cookie *http.Cookie, csrf, origin string) *httptest.ResponseRecorder {
	var input string
	if body != nil {
		b, _ := json.Marshal(body)
		input = string(b)
	}
	r := httptest.NewRequest(method, path, strings.NewReader(input))
	r.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if csrf != "" {
		r.Header.Set("X-CSRF-Token", csrf)
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func object(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var v map[string]any
	if e := json.Unmarshal(w.Body.Bytes(), &v); e != nil {
		t.Fatal(e, w.Body.String())
	}
	return v
}
func setupAccount(t *testing.T, h *Handler, c Config) (*http.Cookie, string) {
	t.Helper()
	token, e := os.ReadFile(c.SetupTokenFile)
	if e != nil {
		t.Fatal(e)
	}
	w := request(h, "POST", "/api/setup", map[string]string{"username": "admin", "password": "test-management-password", "setup_token": strings.TrimSpace(string(token))}, nil, "", "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal("invalid session cookie")
	}
	return cookies[0], object(t, w)["csrf"].(string)
}
func TestFirstAccountAndPersistentSession(t *testing.T) {
	h, c := adminFixture(t)
	if w := request(h, "GET", "/api/stats", nil, nil, "", ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := request(h, "POST", "/api/setup", map[string]string{}, nil, "", "http://attacker.example"); w.Code != 403 {
		t.Fatal("cross origin setup allowed")
	}
	if w := request(h, "POST", "/api/setup", map[string]string{"username": "admin", "password": "test-management-password", "setup_token": "wrong"}, nil, "", ""); w.Code != 403 {
		t.Fatal(w.Code)
	}
	cookie, csrf := setupAccount(t, h, c)
	if _, e := os.Stat(c.SetupTokenFile); !os.IsNotExist(e) {
		t.Fatal("setup token not removed")
	}
	b, _ := os.ReadFile(c.UsersFile)
	if strings.Contains(string(b), "test-management-password") {
		t.Fatal("plaintext password persisted")
	}
	a, e := loadAccount(c.UsersFile)
	if e != nil || !a.verify("admin", "test-management-password") || a.verify("admin", "wrong") {
		t.Fatal("password hashing failed")
	}
	st, _ := os.Stat(c.UsersFile)
	if st.Mode().Perm() != 0600 {
		t.Fatal("credential permissions")
	}
	restarted, e := New(c, h.view)
	if e != nil {
		t.Fatal(e)
	}
	if w := request(restarted, "GET", "/api/stats", nil, cookie, "", ""); w.Code != 200 {
		t.Fatal("session did not survive restart", w.Body.String())
	}
	if w := request(restarted, "POST", "/api/logout", map[string]any{}, cookie, "", ""); w.Code != 403 {
		t.Fatal("CSRF missing accepted")
	}
	if w := request(restarted, "POST", "/api/logout", map[string]any{}, cookie, csrf, ""); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	again, e := New(c, h.view)
	if e != nil {
		t.Fatal(e)
	}
	if w := request(again, "GET", "/api/stats", nil, cookie, "", ""); w.Code != 401 {
		t.Fatal("revoked session resurrected")
	}
}
func TestConfigRedactionValidationRevisionAndApplyRollback(t *testing.T) {
	h, c := adminFixture(t)
	cookie, csrf := setupAccount(t, h, c)
	w := request(h, "GET", "/api/config", nil, cookie, "", "")
	v := object(t, w)
	content := v["content"].(string)
	rev := v["revision"].(string)
	if strings.Contains(content, "existing-proxy-secret") || !strings.Contains(content, secretMarker) {
		t.Fatal("BOM-prefixed password leaked")
	}
	save := func(text, revision string, apply bool) *httptest.ResponseRecorder {
		return request(h, "PUT", "/api/config", map[string]any{"content": text, "revision": revision, "apply": apply}, cookie, csrf, "")
	}
	if w = save(content, "stale", false); w.Code != 409 {
		t.Fatal(w.Code)
	}
	if w = save(content+"invalid", rev, false); w.Code != 400 {
		t.Fatal(w.Code)
	}
	before, _ := os.ReadFile(h.view.AppConfigPath)
	h.view.Apply = func() error { return errors.New("bind failed; restored previous proxy") }
	if w = save(content+"# updated\n", rev, true); w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	after, _ := os.ReadFile(h.view.AppConfigPath)
	if string(before) != string(after) {
		t.Fatal("failed apply did not restore file")
	}
	calls := 0
	h.view.Apply = func() error { calls++; return nil }
	if w = save(content+"# updated\n", rev, true); w.Code != 200 || calls != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	after, _ = os.ReadFile(h.view.AppConfigPath)
	if !strings.Contains(string(after), "existing-proxy-secret") || strings.Contains(string(after), secretMarker) {
		t.Fatal("password not preserved")
	}
	st, e := os.Stat(h.view.AppConfigPath + ".bak")
	if e != nil || st.Mode().Perm() != 0600 {
		t.Fatal("backup missing or insecure")
	}
	if w = request(h, "GET", "/api/stats", nil, cookie, "", ""); w.Code != 200 {
		t.Fatal("apply invalidated login")
	}
}
func TestAccountChangeRevokesEverySession(t *testing.T) {
	h, c := adminFixture(t)
	cookie, csrf := setupAccount(t, h, c)
	w := request(h, "POST", "/api/login", map[string]string{"username": "admin", "password": "test-management-password"}, nil, "", "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	second := w.Result().Cookies()[0]
	w = request(h, "PUT", "/api/account", map[string]string{"username": "new-admin", "current_password": "test-management-password", "password": "new-management-password"}, cookie, csrf, "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	for _, s := range []*http.Cookie{cookie, second} {
		if w = request(h, "GET", "/api/stats", nil, s, "", ""); w.Code != 401 {
			t.Fatal("old session still accepted")
		}
	}
	w = request(h, "POST", "/api/login", map[string]string{"username": "new-admin", "password": "new-management-password"}, nil, "", "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
}
func TestExpiredSessionsCleanedAndStop(t *testing.T) {
	h, c := adminFixture(t)
	h.cfg.SessionTimeout = time.Second
	cookie, _ := setupAccount(t, h, c)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.reap(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		files, _ := os.ReadDir(c.SessionDir)
		if len(files) == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	files, _ := os.ReadDir(c.SessionDir)
	if len(files) != 0 {
		t.Fatal("expired session files retained")
	}
	if w := request(h, "GET", "/api/stats", nil, cookie, "", ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
}
func TestConfigTLSPathsAndRateLimit(t *testing.T) {
	h, c := adminFixture(t)
	c.Listen = "0.0.0.0:9081"
	if c.validate() == nil {
		t.Fatal("public HTTP management accepted")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "admin.conf")
	text := "listen = 127.0.0.1:9081\nusers-file = private/users.json\nsession-dir = private/sessions\nsetup-token-file = private/setup.token\nsession-timeout = 45m\n"
	os.WriteFile(path, []byte(text), 0600)
	got, e := LoadConfig(path)
	if e != nil || got.UsersFile != filepath.Join(dir, "private/users.json") || got.SessionTimeout != 45*time.Minute {
		t.Fatal(got, e)
	}
	for i := 0; i < 21; i++ {
		w := request(h, "POST", "/api/setup", map[string]string{"setup_token": fmt.Sprint(i)}, nil, "", "")
		if i == 20 && w.Code != 429 {
			t.Fatal("login attempts not bounded", w.Code)
		}
	}
}

func TestConcurrentFirstAccountCannotOverwrite(t *testing.T) {
	h, c := adminFixture(t)
	other, e := New(c, h.view)
	if e != nil {
		t.Fatal(e)
	}
	token, e := os.ReadFile(c.SetupTokenFile)
	if e != nil {
		t.Fatal(e)
	}
	done := make(chan int, 2)
	for i, handler := range []*Handler{h, other} {
		go func(h *Handler, i int) {
			w := request(h, "POST", "/api/setup", map[string]string{"username": fmt.Sprintf("operator-%d", i), "password": "test-management-password", "setup_token": strings.TrimSpace(string(token))}, nil, "", "")
			done <- w.Code
		}(handler, i)
	}
	a, b := <-done, <-done
	if !((a == 200 && b == 409) || (a == 409 && b == 200)) {
		t.Fatalf("concurrent setup status %d/%d", a, b)
	}
}
func TestHTTPSCookieAndInstanceIsolation(t *testing.T) {
	h, c := adminFixture(t)
	token, _ := os.ReadFile(c.SetupTokenFile)
	w := request(h, "POST", "https://example.com/api/setup", map[string]string{"username": "admin", "password": "test-management-password", "setup_token": strings.TrimSpace(string(token))}, nil, "", "https://example.com")
	if w.Code != 200 || !w.Result().Cookies()[0].Secure {
		t.Fatal("HTTPS cookie is not Secure", w.Code, w.Body.String())
	}
	c.Listen = "127.0.0.1:9082"
	other, e := New(c, h.view)
	if e != nil {
		t.Fatal(e)
	}
	if h.cookie == other.cookie {
		t.Fatal("different management ports share a cookie name")
	}
	if w = request(other, "GET", "/api/stats", nil, w.Result().Cookies()[0], "", ""); w.Code != 401 {
		t.Fatal("cookie crossed management instances")
	}
}

func TestExternalEditDuringValidationIsNotOverwritten(t *testing.T) {
	h, c := adminFixture(t)
	cookie, csrf := setupAccount(t, h, c)
	v := object(t, request(h, "GET", "/api/config", nil, cookie, "", ""))
	h.view.Validate = func(string) error {
		return os.WriteFile(h.view.AppConfigPath, []byte("# external administrator update\n"), 0600)
	}
	w := request(h, "PUT", "/api/config", map[string]any{"content": v["content"], "revision": v["revision"], "apply": false}, cookie, csrf, "")
	if w.Code != 409 {
		t.Fatal("external file edit overwritten", w.Code)
	}
	b, _ := os.ReadFile(h.view.AppConfigPath)
	if string(b) != "# external administrator update\n" {
		t.Fatal("external edit lost")
	}
}
func TestSessionCleanupPreservesUnrelatedFiles(t *testing.T) {
	h, c := adminFixture(t)
	name := filepath.Join(c.SessionDir, strings.Repeat("a", 64)+".json")
	os.WriteFile(name, []byte("unrelated data"), 0600)
	if e := h.cleanSessions(time.Now().Add(48 * time.Hour)); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(name); e != nil {
		t.Fatal("session cleaner deleted unrelated file", e)
	}
}

func TestMergedFrameworkSecretsAreRedactedAndRestoredIndependently(t *testing.T) {
	original := []byte("service test {\n master_args = -password argument-secret\n master_env = SGATE_KEY:environment-secret\n password = \"proxy-secret\"\n}\n")
	redacted := redactConfig(original)
	for _, secret := range []string{"argument-secret", "environment-secret", "proxy-secret"} {
		if strings.Contains(redacted, secret) {
			t.Fatal("merged service secret exposed")
		}
	}
	restored, e := restoreSecret(redacted, original)
	if e != nil || restored != string(original) {
		t.Fatal("secret placeholders restored to wrong fields", e)
	}
	updated := strings.Replace(redacted, "password = \""+secretMarker+"\"", "password = new-proxy-secret", 1)
	restored, e = restoreSecret(updated, original)
	if e != nil || !strings.Contains(restored, "master_args = -password argument-secret") || !strings.Contains(restored, "password = new-proxy-secret") {
		t.Fatal("independent secret edit failed", e)
	}
}
