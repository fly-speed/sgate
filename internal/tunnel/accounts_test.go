package tunnel

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAccountsFileValidation(t *testing.T) {
	for name, content := range map[string]string{
		"valid":          `{"version":1,"accounts":[{"username":"alice","password":"secret-value"},{"username":"bob","password":"other-secret","disabled":true}]}`,
		"duplicate":      `{"version":1,"accounts":[{"username":"alice","password":"secret-value"},{"username":"alice","password":"other-secret"}]}`,
		"valid-empty":    `{"version":1,"accounts":[]}`,
		"unknown":        `{"version":1,"accounts":[{"username":"alice","password":"secret-value","typo":true}]}`,
		"version":        `{"version":2,"accounts":[{"username":"alice","password":"secret-value"}]}`,
		"trailing":       `{"version":1,"accounts":[]} {}`,
		"blank-password": `{"version":1,"accounts":[{"username":"alice","password":""}]}`,
		"blank-username": `{"version":1,"accounts":[{"username":" ","password":"secret-value"}]}`,
		"oversize":       strings.Repeat(" ", maxAccountsFileBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "accounts.json")
			if err := os.WriteFile(file, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			accounts, err := LoadAccounts(file)
			if name == "valid-empty" {
				if err != nil || len(accounts) != 0 {
					t.Fatal(err)
				}
				return
			}
			if name == "valid" {
				if err != nil || len(accounts) != 2 {
					t.Fatal(err, accounts)
				}
				return
			}
			if err == nil {
				t.Fatal("invalid account file accepted")
			}
			if strings.Contains(err.Error(), "secret-value") {
				t.Fatal("credentials exposed in error")
			}
		})
	}
	if _, err := LoadAccounts(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("missing file accepted")
	}
	if _, err := LoadAccounts(t.TempDir()); err == nil {
		t.Fatal("directory accepted")
	}
}

func TestAccountHeadersEncryptIdentityAndBindContext(t *testing.T) {
	key := bytes.Repeat([]byte{8}, 32)
	s, err := NewServer(ServerConfig{Key: key, Accounts: []Account{{Username: "alice", Password: "secret-value"}}, Target: "localhost:80"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	token, err := encryptAccount(key, "alice", "secret-value", "id", "nonce", "timestamp", "0", "/tunnel")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, "v2.") || strings.Contains(token, "alice") || strings.Contains(token, "secret-value") {
		t.Fatal("credential not encrypted")
	}
	user, ok := s.verifyAccount(token, "id", "nonce", "timestamp", "0", "/tunnel")
	if !ok || user != "alice" {
		t.Fatal("account verification failed")
	}
	for _, uri := range []string{"/other", "/tunnel?addr=localhost:80"} {
		if _, ok := s.verifyAccount(token, "id", "nonce", "timestamp", "0", uri); ok {
			t.Fatal("credential accepted for modified URI")
		}
	}
	if _, ok := s.verifyAccount(token, "other-id", "nonce", "timestamp", "0", "/tunnel"); ok {
		t.Fatal("credential accepted for another session")
	}
}

func TestMultiAccountsWSWSSAndSessionIsolation(t *testing.T) {
	for _, secure := range []bool{false, true} {
		name := "WS"
		if secure {
			name = "WSS"
		}
		t.Run(name, func(t *testing.T) { testMultiAccounts(t, secure) })
	}
}

func testMultiAccounts(t *testing.T, secure bool) {
	target := targetBackend(t, "A")
	opts := testOptions()
	opts.NoiseIntervalMin, opts.NoiseIntervalMax = time.Millisecond, 3*time.Millisecond
	s, err := NewServer(ServerConfig{Key: bytes.Repeat([]byte{8}, 32), Password: "legacy-secret", Accounts: []Account{
		{Username: "alice", Password: "alice-secret"}, {Username: "bob", Password: "bob-secret"}, {Username: "disabled", Password: "disabled-secret", Disabled: true},
	}, Target: target, Options: opts})
	if err != nil {
		t.Fatal(err)
	}
	web := httptest.NewUnstartedServer(s)
	if secure {
		web.StartTLS()
	} else {
		web.Start()
	}
	t.Cleanup(func() {
		s.Close()
		web.Close()
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.sessions) != 0 || len(s.nonces) != 0 {
			t.Error("account sessions or nonce cache retained")
		}
	})
	client := func(username, password string) *Client {
		t.Helper()
		cfg := ClientConfig{Key: s.cfg.Key, Username: username, Password: password, URL: "ws" + strings.TrimPrefix(web.URL, "http") + "/tunnel", Options: opts}
		if secure {
			roots := x509.NewCertPool()
			roots.AddCert(web.Certificate())
			cfg.TLSConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
		}
		c, err := NewClient(cfg)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	for _, bad := range [][2]string{{"alice", "bad-secret"}, {"unknown", "alice-secret"}, {"disabled", "disabled-secret"}, {"", "legacy-secret"}} {
		c := client(bad[0], bad[1])
		conn, _, status, err := c.dial(context.Background(), randomID(), false)
		if conn != nil {
			conn.Close()
		}
		if err == nil || status != http.StatusNotFound {
			t.Fatalf("invalid account accepted: %d %v", status, err)
		}
	}
	// Both accounts concurrently carry distinct records over separate sessions.
	for _, name := range []string{"alice", "bob"} {
		for index := 0; index < 8; index++ {
			t.Run(fmt.Sprintf("%s/%d", name, index), func(t *testing.T) {
				t.Parallel()
				c := client(name, name+"-secret")
				id := randomID()
				ws, codec, _, err := c.dial(context.Background(), id, false)
				if err != nil {
					t.Fatal(err)
				}
				local := newSession(context.Background(), "", c.cfg.Options)
				pipe, app := net.Pipe()
				_ = app.SetDeadline(time.Now().Add(8 * time.Second))
				if err := local.addLocal(pipe); err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() { done <- local.transport(ws, codec) }()
				t.Cleanup(func() { local.close(); app.Close(); ws.Close() })
				marker := make([]byte, 1)
				if _, err := io.ReadFull(app, marker); err != nil || string(marker) != "A" {
					t.Fatal("backend mismatch", err)
				}
				if err := exchange(app, bytes.Repeat([]byte(name+"-unique-record"), 8192)); err != nil {
					t.Fatal(err)
				}
				ws.Close()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Fatal("old transport retained")
				}
				waitFor(t, "detached account session", func() bool { s.mu.Lock(); defer s.mu.Unlock(); return !s.sessions[id].inUse })
				s.mu.Lock()
				idle := s.sessions[id].idle
				s.mu.Unlock()
				other := "alice"
				if name == other {
					other = "bob"
				}
				conn, _, status, err := client(other, other+"-secret").dial(context.Background(), id, true)
				if conn != nil {
					conn.Close()
				}
				if err == nil || status != http.StatusConflict {
					t.Fatalf("another account resumed session: %d %v", status, err)
				}
				s.mu.Lock()
				unchanged := s.sessions[id].idle.Equal(idle)
				s.mu.Unlock()
				if !unchanged {
					t.Fatal("rejected account renewed session retention")
				}
				ws, codec, _, err = c.dial(context.Background(), id, true)
				if err != nil {
					t.Fatal(err)
				}
				go func() { done <- local.transport(ws, codec) }()
				if err := exchange(app, []byte(name+"-resumed-record")); err != nil {
					t.Fatal(err)
				}
				local.close()
				app.Close()
				ws.Close()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Fatal("resumed transport retained")
				}
			})
		}
	}
	result := client("alice", "alice-secret").Probe(context.Background())
	if !result.OK {
		t.Fatal("account diagnostic failed", result)
	}
}
