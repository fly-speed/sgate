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
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func targetBackend(t *testing.T, marker string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var conns sync.Map
	var wg sync.WaitGroup
	acceptDone := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(acceptDone)
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			conns.Store(c, true)
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer c.Close()
				defer conns.Delete(c)
				_, _ = io.WriteString(c, marker)
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		<-acceptDone
		conns.Range(func(k, _ any) bool { k.(net.Conn).Close(); return true })
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("backend connections did not exit")
		}
	})
	return listener.Addr().String()
}

func TestSelectableTargetsWSWSSIntegrityAndResumeBinding(t *testing.T) {
	for _, secure := range []bool{false, true} {
		name := "WS"
		if secure {
			name = "WSS"
		}
		t.Run(name, func(t *testing.T) {
			a := targetBackend(t, "A")
			b := targetBackend(t, "B")
			_, port, _ := net.SplitHostPort(b)
			b = net.JoinHostPort("localhost", port)
			opts := testOptions()
			opts.NoiseIntervalMin, opts.NoiseIntervalMax = time.Millisecond, 3*time.Millisecond
			s, err := NewServer(ServerConfig{
				Key: bytes.Repeat([]byte{8}, 32), Password: testPassword,
				Target: a, AllowedTargets: []string{a, b}, Options: opts,
			})
			if err != nil {
				t.Fatal(err)
			}
			var web *httptest.Server
			if secure {
				web = httptest.NewTLSServer(s)
			} else {
				web = httptest.NewServer(s)
			}
			t.Cleanup(func() {
				s.Close()
				web.Close()
				s.mu.Lock()
				defer s.mu.Unlock()
				if len(s.sessions) != 0 || len(s.nonces) != 0 {
					t.Error("server retained sessions or nonce cache")
				}
			})
			base := "ws" + strings.TrimPrefix(web.URL, "http") + "/tunnel"
			clientFor := func(t *testing.T, endpoint string) *Client {
				t.Helper()
				cfg := ClientConfig{Key: s.cfg.Key, Password: testPassword, URL: endpoint, Options: opts}
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
			t.Run("concurrent-backends", func(t *testing.T) {
				for _, tc := range []struct{ name, query, marker, target string }{
					{"default", "", "A", a},
					{"ip", "?addr=" + url.QueryEscape(a), "A", a},
					{"hostname", "?addr=" + url.QueryEscape(b), "B", b},
				} {
					t.Run(tc.name, func(t *testing.T) {
						t.Parallel()
						client := clientFor(t, base+tc.query)
						id := randomID()
						ws, codec, _, err := client.dial(context.Background(), id, false)
						if err != nil {
							t.Fatal(err)
						}
						local := newSession(context.Background(), "", client.cfg.Options)
						pipe, app := net.Pipe()
						_ = app.SetDeadline(time.Now().Add(8 * time.Second))
						if err = local.addLocal(pipe); err != nil {
							t.Fatal(err)
						}
						done := make(chan error, 1)
						go func() { done <- local.transport(ws, codec) }()
						t.Cleanup(func() { local.close(); app.Close() })
						marker := make([]byte, 1)
						if _, err = io.ReadFull(app, marker); err != nil || string(marker) != tc.marker {
							t.Fatalf("wrong backend: %q %v", marker, err)
						}
						if err = exchange(app, bytes.Repeat([]byte(tc.marker+"-payload"), 8192)); err != nil {
							t.Fatal(err)
						}
						ws.Close()
						select {
						case <-done:
						case <-time.After(3 * time.Second):
							t.Fatal("old transport did not stop")
						}
						waitFor(t, "detached session", func() bool { s.mu.Lock(); defer s.mu.Unlock(); return !s.sessions[id].inUse })
						s.mu.Lock()
						idle := s.sessions[id].idle
						s.mu.Unlock()
						other := a
						if tc.target == a {
							other = b
						}
						bad := clientFor(t, base+"?addr="+url.QueryEscape(other))
						if conn, _, status, err := bad.dial(context.Background(), id, true); err == nil || status != 409 {
							if conn != nil {
								conn.Close()
							}
							t.Fatalf("changed target resumed: %d %v", status, err)
						}
						s.mu.Lock()
						unchanged := s.sessions[id].idle.Equal(idle) && s.sessions[id].session.target == tc.target
						s.mu.Unlock()
						if !unchanged {
							t.Fatal("rejected resume changed session")
						}
						ws, codec, _, err = client.dial(context.Background(), id, true)
						if err != nil {
							t.Fatal(err)
						}
						go func() { done <- local.transport(ws, codec) }()
						if err = exchange(app, bytes.Repeat([]byte("resumed-"+tc.marker), 8192)); err != nil {
							t.Fatal(err)
						}
						local.close()
						app.Close()
						ws.Close()
						select {
						case <-done:
						case <-time.After(3 * time.Second):
							t.Fatal("resumed transport did not stop")
						}
					})
				}
			})
			for _, tc := range []struct {
				query  string
				status int
			}{
				{"?addr=127.0.0.1:2", 403}, {"?addr=", 400},
				{"?addr=" + url.QueryEscape(a) + "&addr=" + url.QueryEscape(b), 400},
				{"?addr=http://localhost:80", 400}, {"?addr=localhost:0", 400},
			} {
				client := clientFor(t, base+tc.query)
				if conn, _, status, err := client.dial(context.Background(), randomID(), false); err == nil || status != tc.status {
					if conn != nil {
						conn.Close()
					}
					t.Fatalf("query %q: status=%d err=%v", tc.query, status, err)
				}
			}
		})
	}
}

func TestSelectableTargetValidationAndDisabledSelection(t *testing.T) {
	for _, address := range []string{"127.0.0.1:80", "example.com:443", "[::1]:8088"} {
		if err := validateSelectableTarget(address); err != nil {
			t.Fatal(address, err)
		}
	}
	for _, address := range []string{
		"", ":80", "host:0", "host:65536", "host:http", "host:+80",
		"http://host:80", "bad/name:80", "host:80\n", "host:80,other:81",
	} {
		if err := validateSelectableTarget(address); err == nil {
			t.Fatal("invalid target accepted", address)
		}
	}
	s, err := NewServer(ServerConfig{Key: bytes.Repeat([]byte{8}, 32), Password: testPassword, Target: "127.0.0.1:80"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w := httptest.NewRecorder()
	_, ok := s.selectTarget(w, httptest.NewRequest(http.MethodGet, "/tunnel?addr=127.0.0.1:80", nil))
	if ok || w.Code != 403 {
		t.Fatal("empty allowlist allowed explicit default target")
	}
}

func TestSelectableTargetQueryTamperingCannotAllocateSession(t *testing.T) {
	key := bytes.Repeat([]byte{8}, 32)
	s, err := NewServer(ServerConfig{
		Key: key, Password: testPassword, Target: "127.0.0.1:80",
		AllowedTargets: []string{"127.0.0.1:81"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	headers, err := requestHeaders(key, testPassword, randomID(), randomID(),
		strconv.FormatInt(time.Now().Unix(), 10), "0", "/tunnel?addr=127.0.0.1:80")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/tunnel?addr=127.0.0.1:81", nil)
	request.Header = headers
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, request)
	if w.Code != 404 {
		t.Fatal("tampered query passed authentication", w.Code)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sessions) != 0 || len(s.nonces) != 0 {
		t.Fatal("invalid request retained state")
	}
}
