package tunnel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type transportErrorConn struct {
	net.Conn
	fail  atomic.Bool
	cause error
}

func (c *transportErrorConn) Write(p []byte) (int, error) {
	if c.fail.Load() {
		return 0, c.cause
	}
	return c.Conn.Write(p)
}

type transportErrorListener struct {
	net.Listener
	cause error
}

func (l transportErrorListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &transportErrorConn{Conn: c, cause: l.cause}, nil
}

func TestTransportPreservesOriginalWriterFailure(t *testing.T) {
	cause := errors.New("injected socket write failure")
	opts, err := testOptions().defaults()
	if err != nil {
		t.Fatal(err)
	}
	s := newSession(context.Background(), "", opts)
	defer s.close()
	done := make(chan error, 1)
	h := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			done <- err
			return
		}
		ws.UnderlyingConn().(*transportErrorConn).fail.Store(true)
		done <- s.transport(ws, newCodec(bytes.Repeat([]byte{42}, 32), "id", "client", "server", false))
	}))
	h.Listener = transportErrorListener{h.Listener, cause}
	h.Start()
	defer h.Close()
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	select {
	case err := <-done:
		if !errors.Is(err, cause) {
			t.Fatalf("original writer failure was hidden by reader shutdown: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("writer failure did not release transport")
	}
}

func TestPongMailboxKeepsLatestPayloadBounded(t *testing.T) {
	pongs := make(chan string, 1)
	for i := 0; i < 10000; i++ {
		queueLatestPong(pongs, fmt.Sprint(i))
	}
	if len(pongs) != 1 || <-pongs != "9999" {
		t.Fatal("Pong mailbox did not retain only the latest payload")
	}
}

func TestPingDoesNotBlockAcknowledgementBehindDataWrite(t *testing.T) {
	key := bytes.Repeat([]byte{42}, 32)
	opts := testOptions()
	opts.WriteTimeout, opts.Heartbeat = 5*time.Second, time.Hour
	opts, err := opts.defaults()
	if err != nil {
		t.Fatal(err)
	}
	s := newSession(context.Background(), "", opts)
	if err := s.enqueue(opData, 1, []byte("already received before reconnect")); err != nil {
		t.Fatal(err)
	}
	accepted := make(chan *resumeBlockedConn, 1)
	confirmed, done := make(chan struct{}), make(chan error, 1)
	h := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			done <- err
			return
		}
		c := ws.UnderlyingConn().(*resumeBlockedConn)
		c.release = make(chan struct{})
		c.blocked.Store(true)
		done <- s.transportConfirmed(ws, newCodec(key, "id", "client", "server", false), time.Now().Add(5*time.Second), func() { close(confirmed) })
	}))
	h.Listener = resumeBlockedListener{h.Listener, accepted}
	h.Start()
	t.Cleanup(func() { s.close(); h.Close() })
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	c := <-accepted
	select {
	case <-c.entered:
	case <-time.After(time.Second):
		t.Fatal("data write did not block")
	}
	if err := ws.WriteControl(websocket.PingMessage, []byte("echo"), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	codec := newCodec(key, "id", "client", "server", true)
	if err := ws.WriteMessage(websocket.BinaryMessage, codec.seal((frame{op: opAck, ack: 1}).encode())); err != nil {
		t.Fatal(err)
	}
	select {
	case <-confirmed:
	case <-time.After(time.Second):
		t.Fatal("Ping reply blocked the reader from processing a valid ACK while data write was stalled")
	}
	s.mu.Lock()
	pending := s.pendingBytes
	s.mu.Unlock()
	if pending != 0 {
		t.Fatal("ACK did not release pending data")
	}
	pong := make(chan string, 1)
	ws.SetPongHandler(func(data string) error { pong <- data; return nil })
	c.blocked.Store(false)
	close(c.release)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}()
	defer func() { ws.Close(); <-readDone }()
	select {
	case data := <-pong:
		if data != "echo" {
			t.Fatalf("Pong payload=%q", data)
		}
	case <-time.After(time.Second):
		t.Fatal("queued Pong did not resume after write progress")
	}
}
