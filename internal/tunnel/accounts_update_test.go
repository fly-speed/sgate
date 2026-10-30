package tunnel

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAccountsUpdateRejectsPreviouslyAuthenticatedHandshake(t *testing.T) {
	key := bytes.Repeat([]byte{8}, 32)
	s, err := NewServer(ServerConfig{Key: key, Accounts: []Account{{Username: "alice", Password: "old-secret"}}, Target: "localhost:80"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	headers, err := accountRequestHeaders(key, "alice", "old-secret", randomID(), randomID(), strconv.FormatInt(time.Now().Unix(), 10), "0", "/tunnel")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/tunnel", nil)
	req.Header = headers
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	handshake, ok := s.authenticateRequest(req)
	if !ok {
		t.Fatal("initial authentication failed")
	}
	if err := s.UpdateAccounts([]Account{{Username: "alice", Password: "new-secret"}}); err != nil {
		t.Fatal(err)
	}
	handshake.target = "localhost:80"
	w := httptest.NewRecorder()
	if _, _, ok := s.acquireSession(w, req, handshake); ok || w.Code != 404 {
		t.Fatal("stale handshake bypassed password rotation", w.Code)
	}
	if err := s.UpdateAccounts([]Account{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.authenticateRequest(req); ok {
		t.Fatal("empty account list enabled legacy fallback")
	}
}

func TestAccountsOnlineRevocationWSWSSPreservesOtherUsers(t *testing.T) {
	for _, secure := range []bool{false, true} {
		name := "WS"
		if secure {
			name = "WSS"
		}
		t.Run(name, func(t *testing.T) {
			key := bytes.Repeat([]byte{8}, 32)
			target := targetBackend(t, "A")
			opts := testOptions()
			opts.NoiseIntervalMin, opts.NoiseIntervalMax = time.Millisecond, 3*time.Millisecond
			s, err := NewServer(ServerConfig{Key: key, Accounts: []Account{{Username: "alice", Password: "a-secret"}, {Username: "bob", Password: "b-secret"}}, Target: target, Options: opts})
			if err != nil {
				t.Fatal(err)
			}
			web := httptest.NewUnstartedServer(s)
			if secure {
				web.StartTLS()
			} else {
				web.Start()
			}
			t.Cleanup(func() { s.Close(); web.Close() })
			type link struct {
				app   net.Conn
				local *session
				done  chan struct{}
				id    string
			}
			connect := func(username, password string) *link {
				t.Helper()
				cfg := ClientConfig{Key: key, Username: username, Password: password, URL: "ws" + strings.TrimPrefix(web.URL, "http") + "/tunnel", Options: opts}
				if secure {
					roots := x509.NewCertPool()
					roots.AddCert(web.Certificate())
					cfg.TLSConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
				}
				c, err := NewClient(cfg)
				if err != nil {
					t.Fatal(err)
				}
				id := randomID()
				ws, codec, _, err := c.dial(context.Background(), id, false)
				if err != nil {
					t.Fatal(err)
				}
				pipe, app := net.Pipe()
				local := newSession(context.Background(), "", c.cfg.Options)
				if err := local.addLocal(pipe); err != nil {
					t.Fatal(err)
				}
				done := make(chan struct{})
				go func() { _ = local.transport(ws, codec); close(done) }()
				t.Cleanup(func() {
					local.close()
					app.Close()
					ws.Close()
					select {
					case <-done:
					case <-time.After(3 * time.Second):
						t.Error("account transport retained")
					}
				})
				_ = app.SetDeadline(time.Now().Add(10 * time.Second))
				marker := make([]byte, 1)
				if _, err := io.ReadFull(app, marker); err != nil || string(marker) != "A" {
					t.Fatal("backend marker", err)
				}
				return &link{app, local, done, id}
			}
			alice, bob := connect("alice", "a-secret"), connect("bob", "b-secret")
			s.mu.Lock()
			original := s.sessions[bob.id]
			aliceSession := s.sessions[alice.id].session
			s.mu.Unlock()
			if err := s.UpdateAccounts([]Account{{Username: "alice", Password: "new-secret"}, {Username: "bob", Password: "b-secret"}}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-alice.done:
			case <-time.After(3 * time.Second):
				t.Fatal("rotated account tunnel remained online")
			}
			assertReleased(t, "rotated account resources released", aliceSession)
			alice.local.close()
			assertSocketClosed(t, alice.app)
			if err := exchange(bob.app, bytes.Repeat([]byte("bob-unchanged-record"), 8192)); err != nil {
				t.Fatal(err)
			}
			s.mu.Lock()
			same := s.sessions[bob.id] == original
			s.mu.Unlock()
			if !same {
				t.Fatal("unmodified account lost its session")
			}
			next := connect("alice", "new-secret")
			s.mu.Lock()
			nextSession := s.sessions[next.id].session
			s.mu.Unlock()
			if err := s.UpdateAccounts([]Account{{Username: "alice", Password: "new-secret", Disabled: true}, {Username: "bob", Password: "b-secret"}}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-next.done:
			case <-time.After(3 * time.Second):
				t.Fatal("disabled account tunnel remained online")
			}
			assertReleased(t, "disabled account resources released", nextSession)
			next.local.close()
			assertSocketClosed(t, next.app)
			if err := exchange(bob.app, []byte("after-other-account-disabled")); err != nil {
				t.Fatal(err)
			}
			if err := s.UpdateAccounts([]Account{}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-bob.done:
			case <-time.After(3 * time.Second):
				t.Fatal("deleted account tunnel remained online")
			}
			assertReleased(t, "deleted account resources released", original.session)
			bob.local.close()
			assertSocketClosed(t, bob.app)
			s.mu.Lock()
			remaining := len(s.sessions)
			s.mu.Unlock()
			if remaining != 0 {
				t.Fatal("revoked sessions retained", remaining)
			}
		})
	}
}

func TestFirstAccountSwitchRevokesLegacyAndRechecksHandshake(t *testing.T) {
	key := bytes.Repeat([]byte{8}, 32)
	s, err := NewServer(ServerConfig{Key: key, Password: "legacy-secret", Target: "localhost:80"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	headers, err := requestHeaders(key, "legacy-secret", randomID(), randomID(), strconv.FormatInt(time.Now().Unix(), 10), "0", "/tunnel")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/tunnel", nil)
	req.Header = headers
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	handshake, ok := s.authenticateRequest(req)
	if !ok {
		t.Fatal("legacy mode not authenticated")
	}
	old := newSession(context.Background(), "localhost:80", s.cfg.Options)
	s.mu.Lock()
	s.sessions[randomID()] = &serverEntry{session: old, idle: time.Now()}
	s.mu.Unlock()
	if err := s.UpdateAccounts([]Account{{Username: "alice", Password: "new-secret"}}); err != nil {
		t.Fatal(err)
	}
	assertReleased(t, "legacy session released after first account", old)
	if _, ok := s.authenticateRequest(req); ok {
		t.Fatal("legacy authentication still accepted")
	}
	w := httptest.NewRecorder()
	if _, _, ok := s.acquireSession(w, req, handshake); ok || w.Code != 404 {
		t.Fatal("old handshake bypassed first-account setup")
	}
}

func TestOnlineLegacySwitchRevokesAccountHandshakeAndResources(t *testing.T) {
	key := bytes.Repeat([]byte{8}, 32)
	s, err := NewServer(ServerConfig{Key: key, Password: "legacy-secret", Accounts: []Account{{Username: "alice", Password: "a-secret"}}, Target: "localhost:80"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	headers, err := accountRequestHeaders(key, "alice", "a-secret", randomID(), randomID(), strconv.FormatInt(time.Now().Unix(), 10), "0", "/tunnel")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/tunnel", nil)
	req.Header = headers
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	stale, ok := s.authenticateRequest(req)
	if !ok {
		t.Fatal("account authentication failed")
	}
	old := newSession(context.Background(), "localhost:80", s.cfg.Options)
	s.mu.Lock()
	s.sessions[randomID()] = &serverEntry{username: "alice", session: old, idle: time.Now()}
	s.mu.Unlock()
	if err := s.UseLegacyAuthentication(); err != nil {
		t.Fatal(err)
	}
	assertReleased(t, "old account session released", old)
	w := httptest.NewRecorder()
	if _, _, ok := s.acquireSession(w, req, stale); ok || w.Code != 404 {
		t.Fatal("stale account handshake entered legacy mode")
	}
	if _, ok := s.authenticateRequest(req); ok {
		t.Fatal("account accepted in legacy mode")
	}
	// Empty account arrays still deny all users rather than enabling legacy mode.
	if err := s.UpdateAccounts([]Account{}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	enabled := s.accountMode
	s.mu.Unlock()
	if !enabled {
		t.Fatal("empty list disabled accounts")
	}
	noLegacy, err := NewServer(ServerConfig{Key: key, Accounts: []Account{{Username: "alice", Password: "a-secret"}}, Target: "localhost:80"})
	if err != nil {
		t.Fatal(err)
	}
	defer noLegacy.Close()
	if err := noLegacy.UseLegacyAuthentication(); err == nil {
		t.Fatal("legacy enabled without a password")
	}
	if _, ok := noLegacy.authenticateRequest(req); !ok {
		t.Fatal("failed switch changed account policy")
	}
}
