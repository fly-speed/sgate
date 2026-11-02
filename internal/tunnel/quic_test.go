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
	"sync"
	"testing"
	"time"
)

func TestQUICMultiplexResume(t *testing.T) {
	f := newFixtureWithTransport(t, 2*time.Minute, nil, testOptions(), testOptions(), true)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		c := f.dial(t)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := exchange(c, bytes.Repeat([]byte("udp-tunnel"), 32768)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	c := f.dial(t)
	if err := exchange(c, []byte("before")); err != nil {
		t.Fatal(err)
	}
	accepts := f.accepted.Load()
	f.disconnect()
	waitFor(t, "QUIC detach", func() bool { return !f.isAttached() })
	payload := bytes.Repeat([]byte("resume"), 32768)
	done := make(chan error, 1)
	go func() { done <- exchange(c, payload) }()
	f.blocked.Store(false)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("QUIC resume blocked")
	}
	if f.accepted.Load() != accepts {
		t.Fatal("resume replaced backend connection")
	}
}

func quicTestEndpoint(t *testing.T, handler http.Handler) (string, *tls.Config) {
	t.Helper()
	certificate := httptest.NewTLSServer(nil)
	roots := x509.NewCertPool()
	roots.AddCert(certificate.Certificate())
	listener, err := ListenQUIC("127.0.0.1:0", certificate.TLS, testOptions())
	certificate.Close()
	if err != nil {
		t.Fatal(err)
	}
	web := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	go web.Serve(listener)
	t.Cleanup(func() { web.Close() })
	return "quic://" + listener.Addr().String() + "/tunnel", &tls.Config{RootCAs: roots}
}

func TestQUICAuthenticationAndReverse(t *testing.T) {
	reverse := reverseAddress(t)
	backend := targetBackend(t, "R")
	s, err := NewServer(ServerConfig{Key: bytes.Repeat([]byte{8}, 32), Target: backend, Options: testOptions(), Accounts: []Account{{Username: "alice", Password: testPassword, ReverseMappings: []ReverseMapping{{Listen: reverse, Target: backend}}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.StartReverse(); err != nil {
		t.Fatal(err)
	}
	endpoint, trust := quicTestEndpoint(t, s)
	cfg := ClientConfig{Key: s.cfg.Key, Username: "alice", Password: testPassword, URL: endpoint, TLSConfig: trust, AllowReverse: true, Options: testOptions()}
	for _, failure := range []string{"certificate", "hostname", "password", "key"} {
		t.Run(failure, func(t *testing.T) {
			bad := cfg
			switch failure {
			case "certificate":
				bad.TLSConfig = nil
			case "hostname":
				bad.TLSConfig = trust.Clone()
				bad.TLSConfig.ServerName = "wrong.example.invalid"
			case "password":
				bad.Password = "wrong-password"
			case "key":
				bad.Key = bytes.Repeat([]byte{9}, 32)
			}
			c, err := NewClient(bad)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			ws, _, _, err := c.dial(ctx, randomID(), false)
			if ws != nil {
				ws.Close()
			}
			if err == nil {
				t.Fatal("invalid credentials accepted")
			}
		})
	}
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ws, _, status, err := c.dial(context.Background(), randomID(), true)
	if ws != nil {
		ws.Close()
	}
	if err == nil || status != http.StatusGone {
		t.Fatalf("missing resume session: status=%d err=%v", status, err)
	}
	link := &reverseLink{client: c, id: randomID(), local: newSession(context.Background(), "", c.cfg.Options)}
	link.attach(t, s, false)
	defer func() { link.local.close(); link.wsClose(); <-link.done }()
	conn := dialReverse(t, reverse)
	readMarker(t, conn, "R")
	if err := exchange(conn, bytes.Repeat([]byte("reverse UDP"), 16384)); err != nil {
		t.Fatal(err)
	}
	// Forward and reverse streams operate simultaneously over the same UDP tunnel.
	pipe, app := net.Pipe()
	defer app.Close()
	app.SetDeadline(time.Now().Add(5 * time.Second))
	if err := link.local.addLocal(pipe); err != nil {
		t.Fatal(err)
	}
	readMarker(t, app, "R")
	if err := exchange(app, []byte("forward UDP")); err != nil {
		t.Fatal(err)
	}
	link.wsClose()
	<-link.done
	waitFor(t, "reverse detach", func() bool { s.mu.Lock(); defer s.mu.Unlock(); return !s.sessions[link.id].inUse })
	link.attach(t, s, true)
	if err := exchange(conn, []byte("reverse resumed")); err != nil {
		t.Fatal(err)
	}
	if err := exchange(app, []byte("forward resumed")); err != nil {
		t.Fatal(err)
	}
	link.local.close()
	link.wsClose()
	<-link.done
	waitFor(t, "authenticated QUIC session close", func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.sessions[link.id] == nil })
}

func TestQUICListenerIdlePeerDoesNotBlockAccept(t *testing.T) {
	endpoint, trust := quicTestEndpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	c, _ := NewClient(ClientConfig{Key: bytes.Repeat([]byte{8}, 32), Password: testPassword, URL: endpoint, TLSConfig: trust, Options: testOptions()})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// Open a stream without sending data. The server cannot accept that stream yet.
	idle, err := c.dialQUIC(ctx, endpoint[len("quic://"):len(endpoint)-len("/tunnel")], "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	active, err := c.dialQUIC(ctx, idle.RemoteAddr().String(), "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	active.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.WriteString(active, "GET / HTTP/1.1\r\nHost: test\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 1024)
	if n, err := active.Read(b); err != nil || !bytes.Contains(b[:n], []byte("200 OK")) {
		t.Fatalf("response=%q err=%v", b[:n], err)
	}
}
