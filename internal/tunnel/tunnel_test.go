package tunnel

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const testPassword = "sgate-test-password"

func testOptions() Options {
	return Options{
		BufferBytes:  64 * 1024,
		Heartbeat:    time.Second,
		DialTimeout:  time.Second,
		WriteTimeout: 2 * time.Second,
		Logf:         func(string, ...any) {},
	}
}
func waitFor(t *testing.T, what string, fn func() bool) {
	t.Helper()
	until := time.Now().Add(5 * time.Second)
	for time.Now().Before(until) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

type fixture struct {
	server   *Server
	http     *httptest.Server
	local    net.Listener
	blocked  atomic.Bool
	accepted atomic.Int32
	bytes    atomic.Int64
	backend  net.Listener
	wg       sync.WaitGroup
}

func newFixture(t *testing.T, ttl time.Duration) *fixture {
	t.Helper()
	return newFixtureWithBackend(t, ttl, nil)
}

func newFixtureWithBackend(t *testing.T, ttl time.Duration, handle func(net.Conn)) *fixture {
	t.Helper()
	return newFixtureWithOptions(t, ttl, handle, testOptions(), testOptions())
}

func newFixtureWithOptions(t *testing.T, ttl time.Duration, handle func(net.Conn), clientOpts, serverOpts Options) *fixture {
	t.Helper()
	f := &fixture{}
	var err error
	f.backend, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var connections sync.Map
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		for {
			c, e := f.backend.Accept()
			if e != nil {
				return
			}
			f.accepted.Add(1)
			connections.Store(c, true)
			f.wg.Add(1)
			go func() {
				defer f.wg.Done()
				defer c.Close()
				defer connections.Delete(c)
				if handle != nil {
					handle(c)
					return
				}
				buf := make([]byte, 8192)
				for {
					n, e := c.Read(buf)
					if n > 0 {
						f.bytes.Add(int64(n))
						if _, e := c.Write(buf[:n]); e != nil {
							return
						}
					}
					if e != nil {
						return
					}
				}
			}()
		}
	}()
	key := bytes.Repeat([]byte{8}, 32)
	f.server, err = NewServer(ServerConfig{
		Password:   testPassword,
		Key:        key,
		Target:     f.backend.Addr().String(),
		SessionTTL: ttl,
		Options:    serverOpts,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.http = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.blocked.Load() {
			http.Error(w, "offline", 503)
			return
		}
		f.server.ServeHTTP(w, r)
	}))
	f.local, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(ClientConfig{
		Password:     testPassword,
		Key:          key,
		URL:          "ws" + strings.TrimPrefix(f.http.URL, "http") + "/tunnel",
		ReconnectMin: 10 * time.Millisecond,
		ReconnectMax: 30 * time.Millisecond,
		Options:      clientOpts,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx, f.local) }()
	t.Cleanup(func() {
		cancel()
		select {
		case e := <-done:
			if e != nil {
				t.Error(e)
			}
		case <-time.After(5 * time.Second):
			t.Error("client shutdown blocked")
		}
		f.server.Close()
		f.http.Close()
		f.backend.Close()
		connections.Range(func(k, v any) bool {
			k.(net.Conn).Close()
			return true
		})
		f.wg.Wait()
	})
	waitFor(t, "tunnel", func() bool { return f.isAttached() })
	return f
}
func (f *fixture) isAttached() bool {
	f.server.mu.Lock()
	defer f.server.mu.Unlock()
	for _, e := range f.server.sessions {
		if e.inUse {
			return true
		}
	}
	return false
}
func (f *fixture) disconnect() {
	f.blocked.Store(true)
	f.server.mu.Lock()
	defer f.server.mu.Unlock()
	for _, e := range f.server.sessions {
		e.session.mu.Lock()
		if e.session.active != nil {
			e.session.active.Close()
		}
		e.session.mu.Unlock()
	}
}
func (f *fixture) dial(t *testing.T) net.Conn {
	t.Helper()
	c, e := net.Dial("tcp", f.local.Addr().String())
	if e != nil {
		t.Fatal(e)
	}
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	t.Cleanup(func() { c.Close() })
	return c
}
func exchange(c net.Conn, data []byte) error {
	sent := make(chan error, 1)
	go func() {
		_, err := io.Copy(c, bytes.NewReader(data))
		sent <- err
	}()
	out := make([]byte, len(data))
	_, err := io.ReadFull(c, out)
	if err != nil {
		return err
	}
	if err = <-sent; err != nil {
		return err
	}
	if !bytes.Equal(out, data) {
		return io.ErrUnexpectedEOF
	}
	return nil
}
func TestReconnectPreservesSocketAndBytes(t *testing.T) {
	f := newFixture(t, 2*time.Minute)
	c := f.dial(t)
	if err := exchange(c, []byte("before disconnect")); err != nil {
		t.Fatal(err)
	}
	f.disconnect()
	waitFor(t, "detachment", func() bool { return !f.isAttached() })
	payload := make([]byte, 2*1024*1024)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- exchange(c, payload) }()
	time.Sleep(75 * time.Millisecond)
	f.blocked.Store(false)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("resume failed")
	}
	if err := exchange(c, []byte("after reconnect")); err != nil {
		t.Fatal(err)
	}
	if n := f.accepted.Load(); n != 1 {
		t.Fatalf("backend TCP connection replaced: %d accepts", n)
	}
	if want := int64(len(payload) + len("before disconnect") + len("after reconnect")); f.bytes.Load() != want {
		t.Fatalf("duplicate/missing backend bytes: got %d want %d", f.bytes.Load(), want)
	}
}
func TestMultiplexAndHalfClose(t *testing.T) {
	f := newFixture(t, 2*time.Minute)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		c := f.dial(t)
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if e := exchange(c, bytes.Repeat([]byte{byte(i)}, 100000)); e != nil {
				t.Error(e)
			}
			if e := c.(*net.TCPConn).CloseWrite(); e != nil {
				t.Error(e)
			}
			one := make([]byte, 1)
			if n, e := c.Read(one); n != 0 || e != io.EOF {
				t.Errorf("half-close: n=%d err=%v", n, e)
			}
		}(i)
	}
	wg.Wait()
	if f.accepted.Load() != 8 {
		t.Fatal("streams did not use independent backend sockets")
	}
}
func TestSessionExpiryClosesOldSocketAndAllowsNew(t *testing.T) {
	f := newFixture(t, 100*time.Millisecond)
	c := f.dial(t)
	if err := exchange(c, []byte("old")); err != nil {
		t.Fatal(err)
	}
	f.disconnect()
	waitFor(t, "detachment", func() bool { return !f.isAttached() })
	waitFor(
		t,
		"session expiry",
		func() bool {
			f.server.mu.Lock()
			defer f.server.mu.Unlock()
			return len(f.server.sessions) == 0
		},
	)
	f.blocked.Store(false)
	one := make([]byte, 1)
	if _, e := c.Read(one); e == nil {
		t.Fatal("expired connection stayed open")
	}
	c2 := f.dial(t)
	if e := exchange(c2, []byte("new")); e != nil {
		t.Fatal(e)
	}
	if f.accepted.Load() != 2 {
		t.Fatalf("expected a new backend socket: %d", f.accepted.Load())
	}
}
func TestWrongKeyRejectedAndHTTPHealth(t *testing.T) {
	f := newFixture(t, 2*time.Minute)
	client, err := NewClient(ClientConfig{
		Password: testPassword,
		Key:      bytes.Repeat([]byte{9}, 32),
		URL:      "ws" + strings.TrimPrefix(f.http.URL, "http") + "/tunnel",
		Options:  testOptions(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ws, _, status, e := client.dial(context.Background(), randomID(), false); e == nil {
		ws.Close()
		t.Fatal("wrong key accepted")
	} else if status != 404 {
		t.Fatalf("unexpected status: %d", status)
	}
	response, e := http.Get(f.http.URL)
	if e != nil {
		t.Fatal(e)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal(response.Status)
	}
}

// Simulate an ACK lost with the old WebSocket: resend OPEN and DATA after resume.
func TestLostAcknowledgementDoesNotRepeatApplicationData(t *testing.T) {
	f := newFixture(t, 2*time.Minute)
	client, e := NewClient(ClientConfig{
		Password: testPassword,
		Key:      bytes.Repeat([]byte{8}, 32),
		URL:      "ws" + strings.TrimPrefix(f.http.URL, "http") + "/tunnel",
		Options:  testOptions(),
	})
	if e != nil {
		t.Fatal(e)
	}
	id := randomID()
	ws, crypt, _, e := client.dial(context.Background(), id, false)
	if e != nil {
		t.Fatal(e)
	}
	send := func(f frame) {
		t.Helper()
		if e := ws.WriteMessage(websocket.BinaryMessage, crypt.seal(f.encode())); e != nil {
			t.Fatal(e)
		}
	}
	send(frame{op: opOpen, seq: 1, stream: 1})
	send(frame{op: opData, seq: 2, stream: 1, data: []byte("first")})
	waitFor(t, "first data delivery", func() bool { return f.bytes.Load() == 5 })
	ws.Close()
	waitFor(
		t,
		"old transport exit",
		func() bool {
			f.server.mu.Lock()
			defer f.server.mu.Unlock()
			return !f.server.sessions[id].inUse
		},
	)
	ws, crypt, _, e = client.dial(context.Background(), id, true)
	if e != nil {
		t.Fatal(e)
	}
	defer ws.Close()
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	send(frame{op: opOpen, seq: 1, stream: 1})
	send(frame{op: opData, seq: 2, stream: 1, data: []byte("first")})
	send(frame{op: opData, seq: 3, stream: 1, data: []byte("second")})
	send(frame{op: opFin, seq: 4, stream: 1})
	var out []byte
	for {
		_, p, e := ws.ReadMessage()
		if e != nil {
			t.Fatal(e)
		}
		p, e = crypt.open(p)
		if e != nil {
			t.Fatal(e)
		}
		got, e := decodeFrame(p)
		if e != nil {
			t.Fatal(e)
		}
		if got.op == opData {
			out = append(out, got.data...)
		}
		if got.seq > 0 {
			send(frame{op: opAck, ack: got.seq})
		}
		if got.op == opFin {
			break
		}
	}
	if string(out) != "firstsecond" {
		t.Fatalf("unexpected response %q", out)
	}
	if f.bytes.Load() != 11 || f.accepted.Load() != 1 {
		t.Fatalf("duplicate application side effects: bytes=%d accepts=%d", f.bytes.Load(), f.accepted.Load())
	}
}

func TestBackendUnavailableResetsOnlyAffectedStream(t *testing.T) {
	f := newFixture(t, 2*time.Minute)
	f.backend.Close()
	c := f.dial(t)
	if _, e := c.Write([]byte("unavailable")); e != nil {
		t.Fatal(e)
	}
	var b [1]byte
	if _, e := c.Read(b[:]); e == nil {
		t.Fatal("backend dial failure did not close client socket")
	}
	if !f.isAttached() {
		t.Fatal("backend failure terminated the shared tunnel")
	}
}

func TestTLSCertificateVerification(t *testing.T) {
	f := newFixture(t, 2*time.Minute)
	secure := httptest.NewTLSServer(f.server)
	defer secure.Close()
	cfg := ClientConfig{
		Password: testPassword,
		Key:      bytes.Repeat([]byte{8}, 32),
		URL:      "wss" + strings.TrimPrefix(secure.URL, "https") + "/tunnel",
		Options:  testOptions(),
	}
	client, e := NewClient(cfg)
	if e != nil {
		t.Fatal(e)
	}
	if ws, _, _, e := client.dial(context.Background(), randomID(), false); e == nil {
		ws.Close()
		t.Fatal("untrusted TLS certificate accepted")
	}
	roots := x509.NewCertPool()
	roots.AddCert(secure.Certificate())
	cfg.TLSConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	client, e = NewClient(cfg)
	if e != nil {
		t.Fatal(e)
	}
	ws, crypt, _, e := client.dial(context.Background(), randomID(), false)
	if e != nil {
		t.Fatal(e)
	}
	defer ws.Close()
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	for _, f := range []frame{{op: opOpen, seq: 1, stream: 1}, {op: opData, seq: 2, stream: 1, data: []byte("TLS")}} {
		if e := ws.WriteMessage(websocket.BinaryMessage, crypt.seal(f.encode())); e != nil {
			t.Fatal(e)
		}
	}
	for {
		_, p, e := ws.ReadMessage()
		if e != nil {
			t.Fatal(e)
		}
		p, e = crypt.open(p)
		if e != nil {
			t.Fatal(e)
		}
		got, e := decodeFrame(p)
		if e != nil {
			t.Fatal(e)
		}
		if got.op == opData {
			if string(got.data) != "TLS" {
				t.Fatalf("unexpected TLS payload: %q", got.data)
			}
			break
		}
	}
	cfg.TLSConfig.ServerName = "mismatch.invalid"
	client, e = NewClient(cfg)
	if e != nil {
		t.Fatal(e)
	}
	if bad, _, _, e := client.dial(context.Background(), randomID(), false); e == nil {
		bad.Close()
		t.Fatal("wrong TLS server name accepted")
	}
}

func TestHandshakeReplayRejected(t *testing.T) {
	f := newFixture(t, 2*time.Minute)
	key := bytes.Repeat([]byte{8}, 32)
	id, nonce, ts := randomID(), randomID(), strconv.FormatInt(time.Now().Unix(), 10)
	headers, err := requestHeaders(key, testPassword, id, nonce, ts, "0", "/tunnel")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "ws" + strings.TrimPrefix(f.http.URL, "http") + "/tunnel"
	ws, _, e := websocket.DefaultDialer.Dial(endpoint, headers)
	if e != nil {
		t.Fatal(e)
	}
	defer ws.Close()
	again, response, e := websocket.DefaultDialer.Dial(endpoint, headers)
	if e == nil {
		again.Close()
		t.Fatal("replayed handshake accepted")
	}
	if response == nil {
		t.Fatal("expected HTTP rejection")
	}
	defer response.Body.Close()
	if response.StatusCode != 404 {
		t.Fatalf("replay status=%d", response.StatusCode)
	}
}
