package tunnel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func reverseAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := l.Addr().String()
	l.Close()
	return address
}

type reverseLink struct {
	client  *Client
	local   *session
	id      string
	wsClose func()
	done    chan struct{}
}

func connectReverse(t *testing.T, s *Server, web *httptest.Server, username string, secure bool) *reverseLink {
	t.Helper()
	return connectReverseOptions(t, s, web, username, secure, testOptions())
}

func connectReverseOptions(t *testing.T, s *Server, web *httptest.Server, username string, secure bool, opts Options) *reverseLink {
	t.Helper()
	cfg := ClientConfig{AllowReverse: true, Key: s.cfg.Key, Username: username, Password: testPassword, URL: "ws" + strings.TrimPrefix(web.URL, "http") + "/tunnel", Options: opts}
	if secure {
		roots := x509.NewCertPool()
		roots.AddCert(web.Certificate())
		cfg.TLSConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	}
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	link := &reverseLink{client: c, id: randomID(), local: newSession(context.Background(), "", c.cfg.Options)}
	link.attach(t, s, false)
	t.Cleanup(func() {
		link.local.close()
		link.wsClose()
		select {
		case <-link.done:
		case <-time.After(5 * time.Second):
			t.Error("reverse transport cleanup blocked")
		}
	})
	return link
}
func (l *reverseLink) attach(t *testing.T, s *Server, resume bool) {
	t.Helper()
	ws, codec, _, err := l.client.dial(context.Background(), l.id, resume)
	if err != nil {
		t.Fatal(err)
	}
	l.wsClose = func() { ws.Close() }
	l.done = make(chan struct{})
	done := l.done
	go func() { _ = l.local.transport(ws, codec); close(done) }()
	waitFor(t, "reverse tunnel confirmed", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.sessions[l.id] != nil && s.sessions[l.id].ready
	})
}
func dialReverse(t *testing.T, address string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(10 * time.Second))
	t.Cleanup(func() { c.Close() })
	return c
}
func readMarker(t *testing.T, c net.Conn, want string) {
	t.Helper()
	b := make([]byte, len(want))
	if _, err := io.ReadFull(c, b); err != nil || string(b) != want {
		t.Fatalf("marker=%q want=%q err=%v", b, want, err)
	}
}

func TestReverseMultipleAccountsDuplexResumeAndRevocation(t *testing.T) {
	for _, secure := range []bool{false, true} {
		name := "WS"
		if secure {
			name = "WSS"
		}
		t.Run(name, func(t *testing.T) {
			a, b, forward := targetBackend(t, "A"), targetBackend(t, "B"), targetBackend(t, "F")
			addressA, addressB, addressBob := reverseAddress(t), reverseAddress(t), reverseAddress(t)
			accounts := []Account{
				{Username: "alice", Password: testPassword, ReverseMappings: []ReverseMapping{{Listen: addressA, Target: a}, {Listen: addressB, Target: b}}},
				{Username: "bob", Password: testPassword, ReverseMappings: []ReverseMapping{{Listen: addressBob, Target: b}}},
			}
			s, err := NewServer(ServerConfig{Key: bytes.Repeat([]byte{8}, 32), Accounts: accounts, Target: forward, Options: testOptions()})
			if err != nil {
				t.Fatal(err)
			}
			if err = s.StartReverse(); err != nil {
				t.Fatal(err)
			}
			web := httptest.NewUnstartedServer(s)
			if secure {
				web.StartTLS()
			} else {
				web.Start()
			}
			t.Cleanup(func() { s.Close(); web.Close() })
			bob := connectReverse(t, s, web, "bob", secure)
			// Bob cannot serve Alice's port, even while his tunnel is ready.
			absent := dialReverse(t, addressA)
			assertSocketClosed(t, absent)
			alice := connectReverse(t, s, web, "alice", secure)
			ca, cb, cc := dialReverse(t, addressA), dialReverse(t, addressB), dialReverse(t, addressBob)
			readMarker(t, ca, "A")
			readMarker(t, cb, "B")
			readMarker(t, cc, "B")
			// Forward and reverse streams share a transport without sharing stream IDs.
			pipe, app := net.Pipe()
			defer app.Close()
			app.SetDeadline(time.Now().Add(10 * time.Second))
			if err := alice.local.addLocal(pipe); err != nil {
				t.Fatal(err)
			}
			readMarker(t, app, "F")
			payload := bytes.Repeat([]byte("reverse-duplex-record"), 16384)
			var wg sync.WaitGroup
			for _, conn := range []net.Conn{ca, cb, cc, app} {
				wg.Add(1)
				go func(c net.Conn) {
					defer wg.Done()
					if err := exchange(c, payload); err != nil {
						t.Error(err)
					}
				}(conn)
			}
			wg.Wait()
			alice.wsClose()
			<-alice.done
			waitFor(t, "reverse detach", func() bool { s.mu.Lock(); defer s.mu.Unlock(); return !s.sessions[alice.id].inUse })
			assertSocketClosed(t, dialReverse(t, addressA))
			// Existing business sockets survive replacement of the encrypted transport.
			alice.attach(t, s, true)
			if err := exchange(ca, []byte("after-resume")); err != nil {
				t.Fatal(err)
			}
			accounts[0].ReverseMappings[0].Target = b
			if err = s.UpdateAccounts(accounts); err != nil {
				t.Fatal(err)
			}
			select {
			case <-alice.done:
			case <-time.After(3 * time.Second):
				t.Fatal("mapping update did not revoke old session")
			}
			assertSocketClosed(t, ca)
			if err := exchange(cc, []byte("bob unaffected")); err != nil {
				t.Fatal(err)
			}
			_ = bob
			connectReverse(t, s, web, "alice", secure)
			changed := dialReverse(t, addressA)
			readMarker(t, changed, "B")
			accounts[0].Disabled = true
			if err = s.UpdateAccounts(accounts); err != nil {
				t.Fatal(err)
			}
			assertSocketClosed(t, changed)
			if conn, err := net.DialTimeout("tcp", addressA, 100*time.Millisecond); err == nil {
				conn.Close()
				t.Fatal("disabled account listener still open")
			}
		})
	}
}

func TestReverseConcurrentAdmissionAndHalfClose(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	go func() {
		for {
			c, err := backend.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); data, _ := io.ReadAll(c); c.Write(data) }()
		}
	}()
	address := reverseAddress(t)
	s, err := NewServer(ServerConfig{Key: bytes.Repeat([]byte{8}, 32), Accounts: []Account{{Username: "alice", Password: testPassword, ReverseMappings: []ReverseMapping{{Listen: address, Target: backend.Addr().String()}}}}, Target: "localhost:80", Options: testOptions()})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.StartReverse(); err != nil {
		t.Fatal(err)
	}
	web := httptest.NewServer(s)
	defer web.Close()
	defer s.Close()
	link := connectReverse(t, s, web, "alice", false)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := net.Dial("tcp", address)
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(5 * time.Second))
			conn.Write([]byte("half-close"))
			conn.(*net.TCPConn).CloseWrite()
			data, err := io.ReadAll(conn)
			if err != nil || string(data) != "half-close" {
				t.Errorf("half close %q: %v", data, err)
			}
		}()
	}
	wg.Wait()
	link.local.mu.Lock()
	defer link.local.mu.Unlock()
	if link.local.closed {
		t.Fatal("concurrent opens closed client session")
	}
}

func TestReversePolicyValidationAndBindRollback(t *testing.T) {
	valid := ReverseMapping{Listen: reverseAddress(t), Target: "192.168.1.10:8080"}
	for _, mapping := range []ReverseMapping{{Listen: ":8080", Target: "localhost:80"}, {Listen: "localhost:8080", Target: "localhost:80"}, {Listen: "127.0.0.1:0", Target: "localhost:80"}, {Listen: "127.0.0.1:080", Target: "localhost:80"}, {Listen: valid.Listen, Target: "127.0.0.1:0"}, {Listen: valid.Listen, Target: "http://localhost:80"}} {
		if _, err := EncodeAccounts([]Account{{Username: "a", Password: testPassword, ReverseMappings: []ReverseMapping{mapping}}}); err == nil {
			t.Fatalf("accepted invalid mapping %+v", mapping)
		}
	}
	accounts := []Account{{Username: "a", Password: testPassword, ReverseMappings: []ReverseMapping{valid}}}
	data, err := EncodeAccounts(accounts)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeAccounts(data)
	if err != nil || decoded[0].ReverseMappings[0] != valid {
		t.Fatal("mapping roundtrip failed", err)
	}
	duplicate := append(append([]Account{}, accounts...), Account{Username: "b", Password: testPassword, ReverseMappings: []ReverseMapping{valid}})
	if _, err := EncodeAccounts(duplicate); err == nil {
		t.Fatal("accepted duplicate listener across accounts")
	}
	s, err := NewServer(ServerConfig{Key: bytes.Repeat([]byte{8}, 32), Accounts: accounts, Target: "localhost:80"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.StartReverse(); err != nil {
		t.Fatal(err)
	}
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	next := []Account{{Username: "a", Password: "new-password", ReverseMappings: []ReverseMapping{valid, {Listen: held.Addr().String(), Target: "localhost:80"}}}}
	if err := s.UpdateAccounts(next); err == nil {
		t.Fatal("occupied bind succeeded")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.reverseListeners) != 1 || s.accounts["a"].digest != sha256.Sum256([]byte(testPassword)) {
		t.Fatal("failed update changed running policy")
	}
}

func TestReverseListenerTransferAndFailedReplacement(t *testing.T) {
	address := reverseAddress(t)
	cfg := ServerConfig{Key: bytes.Repeat([]byte{8}, 32), Password: testPassword, Accounts: []Account{{Username: "alice", Password: testPassword, ReverseMappings: []ReverseMapping{{Listen: address, Target: targetBackend(t, "R")}}}}, Target: "localhost:80", Options: testOptions()}
	old, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	if err = old.StartReverse(); err != nil {
		t.Fatal(err)
	}
	oldListener := old.reverseListeners[address]
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	badCfg := cfg
	badCfg.Accounts = []Account{{Username: "alice", Password: testPassword, ReverseMappings: []ReverseMapping{cfg.Accounts[0].ReverseMappings[0], {Listen: occupied.Addr().String(), Target: "localhost:80"}}}}
	bad, err := NewServer(badCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Close()
	if err = bad.StartReverseFrom(old); err == nil {
		t.Fatal("replacement ignored occupied port")
	}
	if old.reverseListeners[address] != oldListener || oldListener.owner.Load() != old {
		t.Fatal("failed replacement moved listener")
	}
	next, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if err = next.StartReverseFrom(old); err != nil {
		t.Fatal(err)
	}
	old.Close()
	if next.reverseListeners[address] != oldListener || oldListener.owner.Load() != next {
		t.Fatal("replacement did not preserve listener")
	}
	web := httptest.NewServer(next)
	defer web.Close()
	link := connectReverse(t, next, web, "alice", false)
	conn := dialReverse(t, address)
	readMarker(t, conn, "R")
	if err = next.UseLegacyAuthentication(); err != nil {
		t.Fatal(err)
	}
	assertSocketClosed(t, conn)
	select {
	case <-link.done:
	case <-time.After(3 * time.Second):
		t.Fatal("legacy switch retained account tunnel")
	}
	if err = next.UpdateAccounts(cfg.Accounts); err != nil {
		t.Fatal(err)
	}
	connectReverse(t, next, web, "alice", false)
	readMarker(t, dialReverse(t, address), "R")
}

func TestReverseCommandsAreRoleBoundAndValidated(t *testing.T) {
	opts, _ := testOptions().defaults()
	for _, setup := range []struct {
		target  string
		receive bool
	}{{"localhost:80", false}, {"", false}, {"localhost:80", true}} {
		opts.receiveReverse = setup.receive
		s := newSession(context.Background(), setup.target, opts)
		if err := s.apply(frame{op: opReverseOpen, stream: reverseStreamBase + 1, data: []byte("localhost:80")}); err == nil {
			t.Error("unauthorized reverse command accepted")
		}
		s.close()
	}
	for _, f := range []frame{
		{op: opReverseOpen, seq: 1, stream: 1, data: []byte("localhost:80")},
		{op: opReverseOpen, seq: 1, stream: reverseStreamBase + 1},
		{op: opReverseOpen, seq: 1, stream: reverseStreamBase + 1, data: []byte("localhost:0")},
		{op: opReverseOpen, seq: 1, stream: reverseStreamBase + 1, data: []byte("localhost:80\n")},
	} {
		if _, err := decodeFrame(f.encode()); err == nil {
			t.Fatal("invalid command accepted", f)
		}
	}
	valid := frame{op: opReverseOpen, seq: 1, stream: reverseStreamBase + 1, data: []byte("[::1]:8080")}
	if _, err := decodeFrame(valid.encode()); err != nil {
		t.Fatal(err)
	}
}

func TestReverseDialFailureDoesNotCloseTunnel(t *testing.T) {
	address := reverseAddress(t)
	cfg := ServerConfig{Key: bytes.Repeat([]byte{8}, 32), Accounts: []Account{{Username: "alice", Password: testPassword, ReverseMappings: []ReverseMapping{{Listen: address, Target: reverseAddress(t)}}}}, Target: "localhost:80", Options: testOptions()}
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.StartReverse(); err != nil {
		t.Fatal(err)
	}
	web := httptest.NewServer(s)
	defer web.Close()
	link := connectReverse(t, s, web, "alice", false)
	for i := 0; i < 3; i++ {
		conn := dialReverse(t, address)
		assertSocketClosed(t, conn)
	}
	link.local.mu.Lock()
	closed := link.local.closed
	link.local.mu.Unlock()
	if closed {
		t.Fatal("dial failure killed tunnel")
	}
}

func TestReverseExplicitWildcardInterfaces(t *testing.T) {
	for _, host := range []string{"0.0.0.0", "::"} {
		t.Run(host, func(t *testing.T) {
			network, loopback := "tcp4", "127.0.0.1"
			if host == "::" {
				network, loopback = "tcp6", "::1"
			}
			probe, err := net.Listen(network, net.JoinHostPort(host, "0"))
			if err != nil {
				t.Skipf("interface unavailable: %v", err)
			}
			_, port, _ := net.SplitHostPort(probe.Addr().String())
			probe.Close()
			address := net.JoinHostPort(host, port)
			s, err := NewServer(ServerConfig{Key: bytes.Repeat([]byte{8}, 32), Accounts: []Account{{Username: "alice", Password: testPassword, ReverseMappings: []ReverseMapping{{Listen: address, Target: targetBackend(t, "W")}}}}, Target: "localhost:80", Options: testOptions()})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if err = s.StartReverse(); err != nil {
				t.Fatal(err)
			}
			web := httptest.NewServer(s)
			defer web.Close()
			connectReverse(t, s, web, "alice", false)
			readMarker(t, dialReverse(t, net.JoinHostPort(loopback, port)), "W")
		})
	}
}
