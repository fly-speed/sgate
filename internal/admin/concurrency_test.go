package admin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConcurrentLoginBoundedAndReplacementAtLimit(t *testing.T) {
	h, c := adminFixture(t)
	h.cfg.MaxSessions = 1
	cookie, _ := setupAccount(t, h, c)
	// All simultaneous requests are rejected while an expensive hash is running.
	h.authGate <- struct{}{}
	const n = 64
	var wg sync.WaitGroup
	codes := make(chan int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- request(
				h,
				"POST",
				"/api/login",
				map[string]string{"username": "admin", "password": "test-management-password"},
				nil,
				"",
				"",
			).Code
		}()
	}
	wg.Wait()
	close(codes)
	<-h.authGate
	for code := range codes {
		if code != 429 {
			t.Fatalf("concurrent overload: %d", code)
		}
	}
	h.mu.Lock()
	h.attempts = make(map[string]attempt)
	h.mu.Unlock()
	login := func(cookie *http.Cookie) *httptest.ResponseRecorder {
		return request(
			h,
			"POST",
			"/api/login",
			map[string]string{"username": "admin", "password": "test-management-password"},
			cookie,
			"",
			"",
		)
	}
	if w := login(nil); w.Code != 500 {
		t.Fatalf("limit not enforced: %d", w.Code)
	}
	w := login(cookie)
	if w.Code != 200 {
		t.Fatalf("reauthentication at limit failed: %d %s", w.Code, w.Body.String())
	}
	next := w.Result().Cookies()[0]
	if next.Value == cookie.Value {
		t.Fatal("session not rotated")
	}
	if w := request(h, "GET", "/api/stats", nil, cookie, "", ""); w.Code != 401 {
		t.Fatal("old session survived replacement")
	}
	restarted, e := New(h.cfg, h.view)
	if e != nil {
		t.Fatal(e)
	}
	if w := request(restarted, "GET", "/api/stats", nil, next, "", ""); w.Code != 200 {
		t.Fatal("replacement did not persist")
	}
	entries, _ := os.ReadDir(c.SessionDir)
	if len(entries) != 1 {
		t.Fatalf("session leak: %d", len(entries))
	}
}

// A slow request body lets expiry/revocation happen after initial admission.
type blockedBody struct {
	io.Reader
	entered, release chan struct{}
	once             sync.Once
}

func (b *blockedBody) Read(p []byte) (int, error) {
	b.once.Do(func() {
		close(b.entered)
		<-b.release
	})
	return b.Reader.Read(p)
}
func (*blockedBody) Close() error { return nil }
func TestExpiredSessionCannotCommitQueuedMutation(t *testing.T) {
	for _, accountChange := range []bool{false, true} {
		t.Run(map[bool]string{false: "config", true: "account"}[accountChange], func(t *testing.T) {
			h, c := adminFixture(t)
			cookie, csrf := setupAccount(t, h, c)
			before, _ := os.ReadFile(h.view.AppConfigPath)
			content := object(t, request(h, "GET", "/api/config", nil, cookie, "", ""))
			path := "/api/config"
			input := map[string]any{
				"content":  content["content"].(string) + "# should not commit\n",
				"revision": content["revision"],
				"apply":    true,
			}
			if accountChange {
				path = "/api/account"
				input = map[string]any{
					"username":         "changed-admin",
					"password":         "changed-management-password",
					"current_password": "test-management-password",
				}
			}
			data, _ := json.Marshal(input)
			body := &blockedBody{
				Reader:  strings.NewReader(string(data)),
				entered: make(chan struct{}),
				release: make(chan struct{}),
			}
			r := httptest.NewRequest("PUT", path, nil)
			r.Body = body
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("X-CSRF-Token", csrf)
			r.AddCookie(cookie)
			w := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				defer close(done)
				h.ServeHTTP(w, r)
			}()
			<-body.entered
			h.mu.Lock()
			b, e := os.ReadFile(h.sessionPath(cookie.Value))
			if e != nil {
				t.Fatal(e)
			}
			var s loginSession
			json.Unmarshal(b, &s)
			s.Expires = time.Now().Add(-time.Second)
			b, _ = json.Marshal(s)
			e = atomicFile(h.sessionPath(cookie.Value), b)
			h.mu.Unlock()
			if e != nil {
				t.Fatal(e)
			}
			close(body.release)
			<-done
			if w.Code != 401 {
				t.Fatalf("expired request committed: %d %s", w.Code, w.Body.String())
			}
			after, _ := os.ReadFile(h.view.AppConfigPath)
			if string(before) != string(after) {
				t.Fatal("expired request modified config")
			}
			a, e := loadAccount(c.UsersFile)
			if e != nil || a.Username != "admin" {
				t.Fatal("expired request modified account")
			}
		})
	}
}

func TestManyPersistentSessionsExpireAcrossRestarts(t *testing.T) {
	h, c := adminFixture(t)
	h.cfg.SessionTimeout = 2 * time.Second
	h.cfg.MaxSessions = 64
	c = h.cfg
	_, _ = setupAccount(t, h, c)
	a, e := loadAccount(c.UsersFile)
	if e != nil {
		t.Fatal(e)
	}
	cookies := make([]*http.Cookie, 0, 64)
	for i := 0; i < 63; i++ {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/api/login", nil)
		if e := h.newSession(w, r, a); e != nil {
			t.Fatal(e)
		}
		cookies = append(cookies, w.Result().Cookies()[0])
	}
	for i := 0; i < 5; i++ {
		h, e = New(c, h.view)
		if e != nil {
			t.Fatal(e)
		}
		for _, cookie := range cookies {
			if w := request(h, "GET", "/api/stats", nil, cookie, "", ""); w.Code != 200 {
				t.Fatalf("restart %d lost session", i)
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func(handler *Handler) {
		handler.reap(ctx)
		close(done)
	}(h)
	defer func() {
		cancel()
		<-done
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		entries, _ := os.ReadDir(c.SessionDir)
		if len(entries) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sessions retained: %d", len(entries))
		}
		time.Sleep(20 * time.Millisecond)
	}
	h, e = New(c, h.view)
	if e != nil {
		t.Fatal(e)
	}
	for _, cookie := range cookies {
		if w := request(h, "GET", "/api/stats", nil, cookie, "", ""); w.Code != 401 {
			t.Fatal("expired session resurrected")
		}
	}
	// Persistence has no temporary-file debris after cleanup.
	entries, _ := os.ReadDir(filepath.Dir(c.UsersFile))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".sgate-") {
			t.Fatal("temporary file retained", entry.Name())
		}
	}
}

func TestConcurrentConfigWritersOneRevision(t *testing.T) {
	h, c := adminFixture(t)
	cookie, csrf := setupAccount(t, h, c)
	v := object(t, request(h, "GET", "/api/config", nil, cookie, "", ""))
	applied := 0
	h.view.Apply = func() error {
		applied++
		return nil
	}
	const count = 32
	start := make(chan struct{})
	codes := make(chan int, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			w := request(
				h,
				"PUT",
				"/api/config",
				map[string]any{"content": v["content"].(string) + "# concurrent save\n", "revision": v["revision"], "apply": true},
				cookie,
				csrf,
				"",
			)
			codes <- w.Code
		}()
	}
	close(start)
	wg.Wait()
	close(codes)
	success, conflict := 0, 0
	for code := range codes {
		switch code {
		case 200:
			success++
		case 409:
			conflict++
		default:
			t.Fatalf("unexpected response %d", code)
		}
	}
	if success != 1 || conflict != count-1 || applied != 1 {
		t.Fatalf("success=%d conflict=%d apply=%d", success, conflict, applied)
	}
	if w := request(h, "GET", "/api/stats", nil, cookie, "", ""); w.Code != 200 {
		t.Fatal("session invalidated by concurrent saves")
	}
	entries, _ := os.ReadDir(filepath.Dir(c.UsersFile))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".sgate-") {
			t.Fatal("validation/atomic file retained", e.Name())
		}
	}
}

func TestActualConcurrentLoginPersistenceAndLogout(t *testing.T) {
	h, c := adminFixture(t)
	h.cfg.MaxSessions = 64
	c = h.cfg
	initial, initialCSRF := setupAccount(t, h, c)
	const count = 16
	start := make(chan struct{})
	responses := make(chan *httptest.ResponseRecorder, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			responses <- request(
				h,
				"POST",
				"/api/login",
				map[string]string{"username": "admin", "password": "test-management-password"},
				nil,
				"",
				"",
			)
		}()
	}
	close(start)
	wg.Wait()
	close(responses)
	type login struct {
		cookie *http.Cookie
		csrf   string
	}
	sessions := []login{{initial, initialCSRF}}
	busy := 0
	for w := range responses {
		switch w.Code {
		case 200:
			sessions = append(sessions, login{w.Result().Cookies()[0], object(t, w)["csrf"].(string)})
		case 429:
			busy++
		default:
			t.Fatalf("concurrent login: %d %s", w.Code, w.Body.String())
		}
	}
	if len(sessions) == 1 {
		t.Fatal("burst admitted no login")
	}
	restarted, e := New(c, h.view)
	if e != nil {
		t.Fatal(e)
	}
	files, _ := os.ReadDir(c.SessionDir)
	if len(files) != len(sessions) {
		t.Fatalf("session files=%d successful sessions=%d", len(files), len(sessions))
	}
	codes := make(chan int, len(sessions))
	for _, s := range sessions {
		wg.Add(1)
		go func(s login) {
			defer wg.Done()
			w := request(restarted, "GET", "/api/stats", nil, s.cookie, "", "")
			if w.Code != 200 {
				codes <- w.Code
				return
			}
			codes <- request(restarted, "POST", "/api/logout", nil, s.cookie, s.csrf, "").Code
		}(s)
	}
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != 200 {
			t.Fatal("concurrent session/logout", code)
		}
	}
	files, _ = os.ReadDir(c.SessionDir)
	if len(files) != 0 {
		t.Fatalf("logout files retained: %d", len(files))
	}
	again, e := New(c, h.view)
	if e != nil {
		t.Fatal(e)
	}
	for _, s := range sessions {
		if w := request(again, "GET", "/api/stats", nil, s.cookie, "", ""); w.Code != 401 {
			t.Fatal("logged-out session resurrected")
		}
	}
	t.Logf(
		"concurrent requests=%d successful=%d busy=%d persisted and revoked=%d",
		count,
		len(sessions)-1,
		busy,
		len(sessions),
	)
}
