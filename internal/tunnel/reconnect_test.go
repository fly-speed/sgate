package tunnel

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type droppingConn struct {
	net.Conn
	drop *atomic.Bool
}

func (c droppingConn) Write(p []byte) (int, error) {
	if c.drop.Load() {
		return len(p), nil
	}
	return c.Conn.Write(p)
}

func TestDownstreamLostACKIsDeduplicatedAndBuffersReleased(t *testing.T) {
	f := newFixture(t, 2*time.Minute)
	opts, _ := testOptions().defaults()
	s := newSession(context.Background(), "", opts)
	defer s.close()
	local, app := net.Pipe()
	defer app.Close()
	_ = app.SetDeadline(time.Now().Add(5 * time.Second))
	if err := s.addLocal(local); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{8}, 32)
	id := randomID()
	var drop atomic.Bool
	connect := func(resume string) (*websocket.Conn, chan error) {
		t.Helper()
		nonce, ts := randomID(), strconv.FormatInt(time.Now().Unix(), 10)
		h, err := requestHeaders(key, testPassword, id, nonce, ts, resume, "/tunnel")
		if err != nil {
			t.Fatal(err)
		}
		d := websocket.Dialer{NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return droppingConn{c, &drop}, nil
		}}
		ws, r, err := d.Dial("ws"+strings.TrimPrefix(f.http.URL, "http")+"/tunnel", h)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- s.transport(ws, newCodec(key, id, nonce, r.Header.Get("X-Sgate-Nonce"), true)) }()
		return ws, done
	}
	ws, done := connect("0")
	t.Cleanup(func() {
		s.close()
		ws.Close()
	})
	waitFor(t, "backend stream", func() bool { return f.accepted.Load() == 1 })
	f.server.mu.Lock()
	remote := f.server.sessions[id].session
	f.server.mu.Unlock()
	// No application writes occur while ACKs are dropped; the downstream alone is active.
	drop.Store(true)
	first := []byte("response whose acknowledgement is lost")
	if err := remote.enqueue(opData, 1, first); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(first))
	if _, err := io.ReadFull(app, got); err != nil || !bytes.Equal(got, first) {
		t.Fatalf("first response: %q %v", got, err)
	}
	ws.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("old client transport leaked")
	}
	waitFor(
		t,
		"server detach",
		func() bool {
			f.server.mu.Lock()
			defer f.server.mu.Unlock()
			return !f.server.sessions[id].inUse
		},
	)
	remote.mu.Lock()
	retained := len(remote.pending) > 0
	remote.mu.Unlock()
	if !retained {
		t.Fatal("test failed to retain an unacknowledged response")
	}
	drop.Store(false)
	ws, done = connect("1")
	second := []byte("next response")
	if err := remote.enqueue(opData, 1, second); err != nil {
		t.Fatal(err)
	}
	got = make([]byte, len(second))
	if _, err := io.ReadFull(app, got); err != nil || !bytes.Equal(got, second) {
		t.Fatalf("duplicate or missing downstream bytes: %q %v", got, err)
	}
	waitFor(t, "both retransmission buffers released", func() bool {
		s.mu.Lock()
		a := s.pending == nil && s.pendingBytes == 0
		s.mu.Unlock()
		remote.mu.Lock()
		b := remote.pending == nil && remote.pendingBytes == 0
		remote.mu.Unlock()
		return a && b
	})
	s.close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("new client transport leaked")
	}
}

func TestUnauthenticatedGoneDoesNotDiscardSession(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(
			w,
			"proxy error",
			http.StatusGone,
		)
	}))
	defer proxy.Close()
	c, err := NewClient(ClientConfig{
		Password: testPassword,
		Key:      bytes.Repeat([]byte{8}, 32),
		URL:      "ws" + strings.TrimPrefix(proxy.URL, "http") + "/tunnel",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, status, err := c.dial(context.Background(), randomID(), true); err == nil || status == http.StatusGone {
		t.Fatalf("untrusted HTTP 410 was treated as session loss: status=%d err=%v", status, err)
	}
}

func TestRepeatedDisconnectDuringIndependentBidirectionalTraffic(t *testing.T) {
	testIndependentBidirectionalTraffic(t, testOptions())
}

func TestNoiseIndependentBidirectionalTraffic(t *testing.T) {
	opts := testOptions()
	opts.NoiseIntervalMin = time.Millisecond
	opts.NoiseIntervalMax = 5 * time.Millisecond
	testIndependentBidirectionalTraffic(t, opts)
}

func testIndependentBidirectionalTraffic(t *testing.T, opts Options) {
	t.Helper()
	upload := make([]byte, 512*1024)
	download := make([]byte, 768*1024)
	_, _ = rand.Read(upload)
	_, _ = rand.Read(download)
	backendDone := make(chan error, 1)
	pacedWrite := func(c net.Conn, p []byte) error {
		for len(p) > 0 {
			n := 1024
			if len(p) < n {
				n = len(p)
			}
			if _, err := io.Copy(c, bytes.NewReader(p[:n])); err != nil {
				return err
			}
			p = p[n:]
			time.Sleep(100 * time.Microsecond)
		}
		return nil
	}
	f := newFixtureWithOptions(t, 2*time.Minute, func(c net.Conn) {
		sent := make(chan error, 1)
		go func() { sent <- pacedWrite(c, download) }()
		got, err := io.ReadAll(c)
		if err == nil && !bytes.Equal(got, upload) {
			err = errors.New("upstream bytes lost or duplicated")
		}
		if e := <-sent; err == nil {
			err = e
		}
		if e := c.(*net.TCPConn).CloseWrite(); err == nil {
			err = e
		}
		backendDone <- err
	}, opts, opts)
	c := f.dial(t)
	waitFor(t, "backend", func() bool { return f.accepted.Load() == 1 })
	sent := make(chan error, 1)
	go func() {
		err := pacedWrite(c, upload)
		if e := c.(*net.TCPConn).CloseWrite(); err == nil {
			err = e
		}
		sent <- err
	}()
	received := make(chan error, 1)
	go func() {
		got, err := io.ReadAll(c)
		if err == nil && !bytes.Equal(got, download) {
			err = errors.New("downstream bytes lost or duplicated")
		}
		received <- err
	}()
	for i := 0; i < 8; i++ {
		time.Sleep(20 * time.Millisecond)
		f.disconnect()
		waitFor(t, "disconnect", func() bool { return !f.isAttached() })
		time.Sleep(5 * time.Millisecond)
		f.blocked.Store(false)
		waitFor(t, "reconnect", f.isAttached)
	}
	for _, done := range []chan error{sent, received, backendDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("bidirectional transfer or half-close stalled")
		}
	}
	if f.accepted.Load() != 1 {
		t.Fatalf("backend socket changed across reconnects: %d", f.accepted.Load())
	}
	waitFor(t, "streams and replay cache released", func() bool {
		f.server.mu.Lock()
		defer f.server.mu.Unlock()
		for _, e := range f.server.sessions {
			e.session.mu.Lock()
			clean := len(e.session.streams) == 0 && e.session.pending == nil && e.session.pendingBytes == 0
			e.session.mu.Unlock()
			if !clean {
				return false
			}
		}
		return true
	})
}
