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
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAllowedTargetsOnlineWSWSS(t *testing.T) {
	for _, secure := range []bool{false, true} {
		name := "WS"
		if secure {
			name = "WSS"
		}
		t.Run(name, func(t *testing.T) {
			target := targetBackend(t, "A")
			s, err := NewServer(ServerConfig{Key: bytes.Repeat([]byte{8}, 32), Password: testPassword, Target: target, Options: testOptions()})
			if err != nil {
				t.Fatal(err)
			}
			web := httptest.NewUnstartedServer(s)
			if secure {
				web.StartTLS()
			} else {
				web.Start()
			}
			defer web.Close()
			defer s.Close()
			cfg := ClientConfig{Key: s.cfg.Key, Password: testPassword, URL: "ws" + strings.TrimPrefix(web.URL, "http") + "/tunnel", Addr: target, Options: testOptions()}
			if secure {
				roots := x509.NewCertPool()
				roots.AddCert(web.Certificate())
				cfg.TLSConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
			}
			c, err := NewClient(cfg)
			if err != nil {
				t.Fatal(err)
			}
			rejected := func(id string, resume bool) {
				t.Helper()
				conn, _, status, err := c.dial(context.Background(), id, resume)
				if conn != nil {
					conn.Close()
				}
				if err == nil || status != 403 {
					t.Fatalf("removed target accepted: %d %v", status, err)
				}
			}
			id := randomID()
			rejected(id, false)
			if err := s.UpdateAllowedTargets([]string{target}); err != nil {
				t.Fatal(err)
			}
			ws, codec, _, err := c.dial(context.Background(), id, false)
			if err != nil {
				t.Fatal(err)
			}
			local := newSession(context.Background(), "", c.cfg.Options)
			pipe, app := net.Pipe()
			defer app.Close()
			defer local.close()
			_ = app.SetDeadline(time.Now().Add(8 * time.Second))
			if err := local.addLocal(pipe); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- local.transport(ws, codec) }()
			marker := make([]byte, 1)
			if _, err := io.ReadFull(app, marker); err != nil || string(marker) != "A" {
				t.Fatalf("backend marker: %q %v", marker, err)
			}
			if err := s.UpdateAllowedTargets([]string{"localhost:0"}); err == nil {
				t.Fatal("invalid policy accepted")
			}
			// Failed validation keeps the previous policy intact.
			s.mu.Lock()
			_, allowed := s.allowedTargets[target]
			s.mu.Unlock()
			if !allowed {
				t.Fatal("invalid update changed policy")
			}
			if err := s.UpdateAllowedTargets(nil); err != nil {
				t.Fatal(err)
			}
			rejected(randomID(), false)
			if err := exchange(app, bytes.Repeat([]byte("existing-business"), 4096)); err != nil {
				t.Fatal(err)
			}
			ws.Close()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("transport retained")
			}
			waitFor(t, "detached session", func() bool { s.mu.Lock(); defer s.mu.Unlock(); return !s.sessions[id].inUse })
			rejected(id, true)
			if err := s.UpdateAllowedTargets([]string{target}); err != nil {
				t.Fatal(err)
			}
			ws, codec, _, err = c.dial(context.Background(), id, true)
			if err != nil {
				t.Fatal(err)
			}
			go func() { done <- local.transport(ws, codec) }()
			if err := exchange(app, bytes.Repeat([]byte("resumed-business"), 4096)); err != nil {
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
			// Empty selection policy still permits the configured default backend.
			if err := s.UpdateAllowedTargets(nil); err != nil {
				t.Fatal(err)
			}
			cfg.Addr = ""
			defaultClient, err := NewClient(cfg)
			if err != nil {
				t.Fatal(err)
			}
			conn, _, _, err := defaultClient.dial(context.Background(), randomID(), false)
			if err != nil {
				t.Fatal(err)
			}
			conn.Close()
		})
	}
}

// Requests may run concurrently with management saves on the shared listener.
func TestAllowedTargetsConcurrentUpdates(t *testing.T) {
	s, err := NewServer(ServerConfig{Key: bytes.Repeat([]byte{8}, 32), Password: testPassword, Target: "localhost:8080"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 200; n++ {
				w := httptest.NewRecorder()
				target, ok := s.selectTarget(w, httptest.NewRequest(http.MethodGet, "/tunnel?addr=localhost:8080", nil))
				if (ok && target != "localhost:8080") || (!ok && w.Code != http.StatusForbidden) {
					t.Errorf("unexpected selection: %q %v %d", target, ok, w.Code)
					return
				}
			}
		}()
	}
	for n := 0; n < 200; n++ {
		targets := []string{"localhost:8080"}
		if n%2 == 0 {
			targets = nil
		}
		if err := s.UpdateAllowedTargets(targets); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
	s.Close()
	if err := s.UpdateAllowedTargets([]string{"localhost:8080"}); err == nil {
		t.Fatal("stopped server accepted update")
	}
}
