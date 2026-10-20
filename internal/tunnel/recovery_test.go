package tunnel

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type recoveryFixture struct {
	t                        *testing.T
	server                   *Server
	http                     *httptest.Server
	addr                     string
	cfg                      ServerConfig
	local                    net.Listener
	state                    clientSessions
	blocked, stalled         atomic.Bool
	dropResponses            atomic.Bool
	backendCount, localCount connectionCounter
	backendBytes             atomic.Int64
	backend                  net.Listener
	backendConns             sync.Map
	backendWG                sync.WaitGroup
	cancel                   context.CancelFunc
	done                     chan error
	tls                      bool
}

type recoveryListener struct {
	net.Listener
	drop *atomic.Bool
}

func (l recoveryListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return droppingConn{c, l.drop}, nil
}

type readCounter struct {
	io.Reader
	count *atomic.Int64
}

func (r readCounter) Read(p []byte) (int, error) {
	n, e := r.Reader.Read(p)
	r.count.Add(int64(n))
	return n, e
}
func newRecoveryFixture(t *testing.T, ttl, offline time.Duration, initialMode string, configure ...func(*ClientConfig)) *recoveryFixture {
	t.Helper()
	return newRecoveryFixtureOptions(t, ttl, offline, initialMode, testOptions(), configure...)
}

func newRecoveryFixtureOptions(t *testing.T, ttl, offline time.Duration, initialMode string, options Options, configure ...func(*ClientConfig)) *recoveryFixture {
	t.Helper()
	f := &recoveryFixture{t: t, done: make(chan error, 1)}
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	f.backend = countedListener{listener, &f.backendCount}
	f.backendWG.Add(1)
	go func() {
		defer f.backendWG.Done()
		for {
			c, e := f.backend.Accept()
			if e != nil {
				return
			}
			f.backendConns.Store(c, true)
			f.backendWG.Add(1)
			go func() {
				defer f.backendWG.Done()
				defer c.Close()
				defer f.backendConns.Delete(c)
				_, _ = io.Copy(c, readCounter{c, &f.backendBytes})
			}()
		}
	}()
	f.cfg = ServerConfig{Password: testPassword, Key: bytes.Repeat([]byte{27}, 32), Target: f.backend.Addr().String(), SessionTTL: ttl, Options: options}
	f.blocked.Store(initialMode == "offline")
	f.stalled.Store(initialMode == "stall")
	f.tls = initialMode == "tls"
	f.startServer("")
	local, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	f.local = countedListener{local, &f.localCount}
	clientConfig := ClientConfig{Password: testPassword, Key: f.cfg.Key, URL: strings.Replace(f.http.URL, "http", "ws", 1) + "/tunnel", OfflineTimeout: offline, ReconnectMin: 10 * time.Millisecond, ReconnectMax: 30 * time.Millisecond, Options: options}
	if f.tls {
		roots := x509.NewCertPool()
		roots.AddCert(f.http.Certificate())
		clientConfig.TLSConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	}
	for _, configure := range configure {
		configure(&clientConfig)
	}
	client, e := NewClient(clientConfig)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	go func() { f.done <- client.run(ctx, f.local, &f.state) }()
	t.Cleanup(func() {
		cancel()
		select {
		case e := <-f.done:
			if e != nil {
				t.Error(e)
			}
		case <-time.After(5 * time.Second):
			t.Error("client did not stop")
		}
		f.stopServer()
		f.backend.Close()
		f.backendConns.Range(func(k, v any) bool { k.(net.Conn).Close(); return true })
		f.backendWG.Wait()
	})
	waitFor(t, "initial client session", func() bool { return f.clientSession() != nil })
	if initialMode == "" || initialMode == "tls" {
		waitFor(t, "initial recovery transport", f.attached)
	}
	return f
}
func (f *recoveryFixture) startServer(addr string) {
	f.t.Helper()
	s, e := NewServer(f.cfg)
	if e != nil {
		f.t.Fatal(e)
	}
	f.server = s
	h := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.stalled.Load() {
			<-r.Context().Done()
			return
		}
		if f.blocked.Load() {
			http.Error(w, "offline", 503)
			return
		}
		s.ServeHTTP(w, r)
	}))
	if addr != "" {
		h.Listener.Close()
		l, e := net.Listen("tcp", addr)
		if e != nil {
			f.t.Fatal(e)
		}
		h.Listener = l
	}
	f.http = h
	f.addr = h.Listener.Addr().String()
	h.Listener = recoveryListener{h.Listener, &f.dropResponses}
	if f.tls {
		h.StartTLS()
	} else {
		h.Start()
	}
}
func (f *recoveryFixture) stopServer() {
	if f.server != nil {
		f.server.Close()
	}
	if f.http != nil {
		f.http.Close()
	}
}
func (f *recoveryFixture) clientSession() *session {
	f.state.mu.Lock()
	defer f.state.mu.Unlock()
	return f.state.current
}
func (f *recoveryFixture) remoteSession() *session {
	f.server.mu.Lock()
	defer f.server.mu.Unlock()
	for _, e := range f.server.sessions {
		if e.inUse {
			return e.session
		}
	}
	return nil
}
func (f *recoveryFixture) attached() bool { return f.remoteSession() != nil }
func (f *recoveryFixture) disconnect() {
	f.blocked.Store(true)
	f.server.mu.Lock()
	for _, e := range f.server.sessions {
		e.session.mu.Lock()
		if e.session.active != nil {
			e.session.active.Close()
		}
		e.session.mu.Unlock()
	}
	f.server.mu.Unlock()
	waitFor(f.t, "server detach", func() bool { return !f.attached() })
}
func (f *recoveryFixture) dial() net.Conn {
	f.t.Helper()
	c, e := net.Dial("tcp", f.local.Addr().String())
	if e != nil {
		f.t.Fatal(e)
	}
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	f.t.Cleanup(func() { c.Close() })
	return c
}
func sessionReleased(s *session) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed && len(s.streams) == 0 && s.pending == nil && s.pendingBytes == 0 && s.active == nil
}
func assertReleased(t *testing.T, what string, s *session) {
	t.Helper()
	waitFor(t, what, func() bool { return sessionReleased(s) })
}
func assertSocketClosed(t *testing.T, c net.Conn) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	var b [1]byte
	n, e := c.Read(b[:])
	if n != 0 || e == nil {
		t.Fatalf("old connection returned data or remained open: n=%d err=%v", n, e)
	}
	if e, ok := e.(net.Error); ok && e.Timeout() {
		t.Fatal("old connection cleanup timed out")
	}
}

// Populate real upstream and downstream queues while the transport is down.
func (f *recoveryFixture) fillQueues(app net.Conn, client, remote *session) {
	f.t.Helper()
	var backend net.Conn
	f.backendConns.Range(func(k, v any) bool { backend = k.(net.Conn); return false })
	if backend == nil {
		f.t.Fatal("missing backend")
	}
	payload := bytes.Repeat([]byte{0xdb}, 256*1024)
	writes := []net.Conn{app, backend}
	for _, c := range writes {
		go func(c net.Conn) { _, _ = c.Write(payload) }(c)
	}
	waitFor(f.t, "full bidirectional replay queues", func() bool {
		client.mu.Lock()
		a := client.pendingBytes > 32*1024
		client.mu.Unlock()
		remote.mu.Lock()
		b := remote.pendingBytes > 32*1024
		remote.mu.Unlock()
		return a && b
	})
}
func (f *recoveryFixture) assertFreshConnection() {
	f.t.Helper()
	waitFor(f.t, "fresh transport", f.attached)
	before := f.backendBytes.Load()
	app := f.dial()
	fresh := []byte("fresh-connection-after-recovery")
	if e := exchange(app, fresh); e != nil {
		f.t.Fatal(e)
	}
	if e := app.(*net.TCPConn).CloseWrite(); e != nil {
		f.t.Fatal(e)
	}
	var b [1]byte
	if n, e := app.Read(b[:]); n != 0 || e != io.EOF {
		f.t.Fatalf("new connection tail: %d %v", n, e)
	}
	app.Close()
	if got := f.backendBytes.Load() - before; got != int64(len(fresh)) {
		f.t.Fatalf("old cache replayed into new stream: %d backend bytes", got)
	}
	waitFor(f.t, "fresh stream cleanup", func() bool {
		if f.backendCount.active.Load() != 0 || f.localCount.active.Load() != 0 {
			return false
		}
		s := f.remoteSession()
		if s == nil {
			return false
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.streams) == 0 && s.pending == nil && s.pendingBytes == 0
	})
}

func TestLongOutageReleasesStateBeforeNetworkReturns(t *testing.T) {
	f := newRecoveryFixture(t, 500*time.Millisecond, 350*time.Millisecond, "")
	app := f.dial()
	if e := exchange(app, []byte("baseline")); e != nil {
		t.Fatal(e)
	}
	client, remote := f.clientSession(), f.remoteSession()
	f.disconnect()
	f.fillQueues(app, client, remote)
	assertReleased(t, "offline client release", client)
	assertReleased(t, "expired server release", remote)
	assertSocketClosed(t, app)
	if !f.blocked.Load() {
		t.Fatal("cleanup required network recovery")
	}
	waitFor(t, "old sockets released offline", func() bool { return f.backendCount.active.Load() == 0 && f.localCount.active.Load() == 0 })
	f.blocked.Store(false)
	f.assertFreshConnection()
}

func TestServerRestartDropsOldQueuesAndAcceptsFreshConnections(t *testing.T) {
	f := newRecoveryFixture(t, time.Minute, 5*time.Second, "")
	app := f.dial()
	if e := exchange(app, []byte("baseline")); e != nil {
		t.Fatal(e)
	}
	client, remote, oldServer := f.clientSession(), f.remoteSession(), f.server
	f.disconnect()
	f.fillQueues(app, client, remote)
	f.stopServer()
	assertReleased(t, "stopped server release", remote)
	oldServer.mu.Lock()
	clean := len(oldServer.sessions) == 0 && len(oldServer.nonces) == 0
	oldServer.mu.Unlock()
	if !clean {
		t.Fatal("stopped server retained caches")
	}
	if c, e := net.DialTimeout("tcp", f.addr, 100*time.Millisecond); e == nil {
		c.Close()
		t.Fatal("old HTTP listener still open")
	}
	// Bring up a new server instance on the very same address, with no old session state.
	f.startServer(f.addr)
	f.blocked.Store(false)
	assertReleased(t, "client release after authenticated 410", client)
	assertSocketClosed(t, app)
	waitFor(t, "old backend socket release", func() bool { return f.backendCount.active.Load() == 0 })
	f.assertFreshConnection()
}

func TestSessionExpiryWithFullBuffersReleasesOldConnections(t *testing.T) {
	f := newRecoveryFixture(t, 200*time.Millisecond, 5*time.Second, "")
	app := f.dial()
	if e := exchange(app, []byte("baseline")); e != nil {
		t.Fatal(e)
	}
	client, remote := f.clientSession(), f.remoteSession()
	f.disconnect()
	f.fillQueues(app, client, remote)
	assertReleased(t, "expired server cache release", remote)
	client.mu.Lock()
	preserved := !client.closed && client.pendingBytes > 0
	client.mu.Unlock()
	if !preserved {
		t.Fatal("client should retain state until its own timeout or authenticated expiry")
	}
	f.blocked.Store(false)
	assertReleased(t, "client release after expiry response", client)
	assertSocketClosed(t, app)
	f.assertFreshConnection()
}

func TestStalledHandshakeCannotExtendOfflineRetention(t *testing.T) {
	f := newRecoveryFixture(t, time.Minute, 200*time.Millisecond, "stall")
	client := f.clientSession()
	app := f.dial()
	if _, e := app.Write(bytes.Repeat([]byte{3}, 128*1024)); e != nil {
		t.Fatal(e)
	}
	assertReleased(t, "offline release during stalled initial handshake", client)
	assertSocketClosed(t, app)
	f.stalled.Store(false)
	f.assertFreshConnection()
}

func TestRepeatedServerRestartsReleaseOldSessions(t *testing.T) {
	f := newRecoveryFixture(t, time.Minute, 5*time.Second, "")
	base := runtime.NumGoroutine()
	for i := 0; i < 5; i++ {
		app := f.dial()
		if e := exchange(app, []byte(fmt.Sprint(i))); e != nil {
			t.Fatal(e)
		}
		client, remote := f.clientSession(), f.remoteSession()
		f.disconnect()
		f.fillQueues(app, client, remote)
		f.stopServer()
		f.startServer(f.addr)
		f.blocked.Store(false)
		assertReleased(t, "old client session", client)
		assertReleased(t, "old server session", remote)
		assertSocketClosed(t, app)
		app.Close()
		f.assertFreshConnection()
	}
	waitFor(t, "restart goroutines returned", func() bool { return runtime.NumGoroutine() <= base+6 })
}

// A wall-clock check of the default two-minute client retention, not a scaled timer.
func TestLongRecoveryDefaultOfflineWindow(t *testing.T) {
	if os.Getenv("SGATE_RECOVERY_LONG") != "1" {
		t.Skip("set SGATE_RECOVERY_LONG=1 for the real two-minute outage test")
	}
	f := newRecoveryFixture(t, 2*time.Minute, 0, "")
	app := f.dial()
	if e := exchange(app, []byte("baseline")); e != nil {
		t.Fatal(e)
	}
	_ = app.SetDeadline(time.Time{})
	client, remote := f.clientSession(), f.remoteSession()
	start := time.Now()
	f.disconnect()
	f.fillQueues(app, client, remote)
	deadline := time.Now().Add(125 * time.Second)
	for !sessionReleased(client) || !sessionReleased(remote) {
		if time.Now().After(deadline) {
			t.Fatal("default offline window failed to release state")
		}
		time.Sleep(250 * time.Millisecond)
	}
	if time.Since(start) < 119*time.Second {
		t.Fatal("default recovery window ended early")
	}
	assertSocketClosed(t, app)
	f.blocked.Store(false)
	f.assertFreshConnection()
	t.Logf("real outage %s: both sessions, sockets and replay buffers released; fresh TCP exchange passed", time.Since(start).Round(time.Millisecond))
}

func TestOutageWithinRetentionPreservesBidirectionalData(t *testing.T) {
	f := newRecoveryFixture(t, 2*time.Second, 2*time.Second, "")
	app := f.dial()
	if e := exchange(app, []byte("baseline")); e != nil {
		t.Fatal(e)
	}
	client, remote := f.clientSession(), f.remoteSession()
	f.disconnect()
	var backend net.Conn
	f.backendConns.Range(func(k, v any) bool { backend = k.(net.Conn); return false })
	up, down := make([]byte, 256*1024), make([]byte, 256*1024)
	_, _ = rand.Read(up)
	_, _ = rand.Read(down)
	writes := make(chan error, 2)
	go func() { _, e := io.Copy(app, bytes.NewReader(up)); writes <- e }()
	go func() { _, e := io.Copy(backend, bytes.NewReader(down)); writes <- e }()
	waitFor(t, "retained queues", func() bool {
		client.mu.Lock()
		a := client.pendingBytes > 32*1024
		client.mu.Unlock()
		remote.mu.Lock()
		b := remote.pendingBytes > 32*1024
		remote.mu.Unlock()
		return a && b
	})
	time.Sleep(50 * time.Millisecond)
	f.blocked.Store(false)
	got := make([]byte, len(up)+len(down))
	if _, e := io.ReadFull(app, got); e != nil {
		t.Fatal(e)
	}
	expected := append(append([]byte(nil), down...), up...)
	if !bytes.Equal(got, expected) {
		t.Fatal("preserved connection lost or repeated bidirectional data")
	}
	for i := 0; i < 2; i++ {
		if e := <-writes; e != nil {
			t.Fatal(e)
		}
	}
	if f.clientSession() != client || f.remoteSession() != remote || f.backendCount.accepted.Load() != 1 {
		t.Fatal("short outage replaced a retained TCP connection")
	}
	if e := app.(*net.TCPConn).CloseWrite(); e != nil {
		t.Fatal(e)
	}
	var b [1]byte
	if n, e := app.Read(b[:]); n != 0 || e != io.EOF {
		t.Fatalf("retained connection tail: %d %v", n, e)
	}
}

func TestBlackholedTransportTimesOutAndReleasesState(t *testing.T) {
	f := newRecoveryFixture(t, 200*time.Millisecond, 250*time.Millisecond, "")
	app := f.dial()
	if e := exchange(app, []byte("baseline")); e != nil {
		t.Fatal(e)
	}
	client, remote := f.clientSession(), f.remoteSession()
	// Silently discard every server-to-client byte. Neither endpoint gets an
	// explicit disconnect signal: missed heartbeat replies must detect the failure.
	f.dropResponses.Store(true)
	f.blocked.Store(true)
	if _, e := app.Write(bytes.Repeat([]byte{17}, 16*1024)); e != nil {
		t.Fatal(e)
	}
	waitFor(t, "unacknowledged data on blackholed transport", func() bool { client.mu.Lock(); defer client.mu.Unlock(); return client.pendingBytes > 0 })
	assertReleased(t, "blackholed client state", client)
	assertReleased(t, "blackholed server state", remote)
	assertSocketClosed(t, app)
	f.dropResponses.Store(false)
	f.blocked.Store(false)
	f.assertFreshConnection()
}

func TestOnlineSessionOutlivesOfflineTimeout(t *testing.T) {
	f := newRecoveryFixture(t, time.Minute, 100*time.Millisecond, "")
	app := f.dial()
	client := f.clientSession()
	until := time.Now().Add(350 * time.Millisecond)
	for time.Now().Before(until) {
		if e := exchange(app, []byte("still online")); e != nil {
			t.Fatal(e)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if f.clientSession() != client || f.backendCount.accepted.Load() != 1 {
		t.Fatal("offline retention closed a healthy online connection")
	}
}

func TestOfflineRetentionClampsReconnectBackoff(t *testing.T) {
	f := newRecoveryFixture(t, time.Minute, 200*time.Millisecond, "offline", func(c *ClientConfig) { c.ReconnectMin = 5 * time.Second; c.ReconnectMax = 5 * time.Second })
	client := f.clientSession()
	app := f.dial()
	start := time.Now()
	if _, e := app.Write(bytes.Repeat([]byte{7}, 128*1024)); e != nil {
		t.Fatal(e)
	}
	assertReleased(t, "offline release during long reconnect backoff", client)
	if time.Since(start) > time.Second {
		t.Fatal("reconnect backoff extended offline retention")
	}
	assertSocketClosed(t, app)
	f.blocked.Store(false)
	f.assertFreshConnection()
}
