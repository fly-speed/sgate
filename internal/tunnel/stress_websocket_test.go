package tunnel

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestStressWebSocketDuplexBaseline(t *testing.T) {
	if os.Getenv("SGATE_STRESS_WS") != "1" {
		t.Skip("set SGATE_STRESS_WS=1 for WebSocket control")
	}
	accepted := make(chan *websocket.Conn, 4)
	h := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{ReadBufferSize: chunkSize, WriteBufferSize: chunkSize}).Upgrade(w, r, nil)
		if err == nil {
			accepted <- ws
		}
	}))
	var count connectionCounter
	h.Listener = countedListener{h.Listener, &count}
	h.Start()
	defer h.Close()
	var conns []*websocket.Conn
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	key := bytes.Repeat([]byte{8}, 32)
	results := make(chan error, 16)
	for i := 0; i < 4; i++ {
		client, _, err := (&websocket.Dialer{ReadBufferSize: chunkSize, WriteBufferSize: chunkSize}).Dial(
			"ws"+strings.TrimPrefix(
				h.URL,
				"http",
			),
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		server := <-accepted
		conns = append(conns, client, server)
		for j, c := range []*websocket.Conn{client, server} {
			crypt := newCodec(key, "session", "client", "server", j == 0)
			if os.Getenv("SGATE_STRESS_SESSION") == "1" {
				opts, err := (Options{
					BufferBytes:      256 * 1024,
					Heartbeat:        5 * time.Second,
					WriteTimeout:     10 * time.Second,
					NoiseIntervalMin: time.Second,
					NoiseIntervalMax: 3 * time.Second,
				}).defaults()
				if err != nil {
					t.Fatal(err)
				}
				s := newSession(context.Background(), "", opts)
				defer s.close()
				go func() { results <- s.transport(c, crypt) }()
				go func() {
					p := bytes.Repeat([]byte{7}, chunkSize)
					for n := 1; n <= 10000; n++ {
						if err := s.enqueue(opData, 1, p); err != nil {
							results <- err
							return
						}
					}
					until := time.Now().Add(30 * time.Second)
					for time.Now().Before(until) {
						s.mu.Lock()
						done := s.received == 10000 && s.pendingBytes == 0
						s.mu.Unlock()
						if done {
							results <- nil
							return
						}
						time.Sleep(time.Millisecond)
					}
					results <- fmt.Errorf("reliable session stalled")
				}()
				continue
			}
			go func() {
				for n := 1; n <= 2048; n++ {
					c.SetWriteDeadline(time.Now().Add(15 * time.Second))
					p := (frame{op: opData, seq: uint64(n), stream: 1, data: bytes.Repeat([]byte{byte(n)}, chunkSize)}).encode()
					if err := c.WriteMessage(websocket.BinaryMessage, crypt.seal(p)); err != nil {
						results <- err
						return
					}
				}
				results <- nil
			}()
			go func() {
				for n := 1; n <= 2048; n++ {
					p, err := readTransportMessage(c, func() error { return c.SetReadDeadline(time.Now().Add(15 * time.Second)) })
					if err != nil {
						results <- err
						return
					}
					p, err = crypt.open(p)
					if err != nil {
						results <- err
						return
					}
					f, err := decodeFrame(p)
					if err != nil || f.seq != uint64(n) || !bytes.Equal(f.data, bytes.Repeat([]byte{byte(n)}, chunkSize)) {
						results <- fmt.Errorf("frame %d corrupt: %v", n, err)
						return
					}
				}
				results <- nil
			}()
		}
	}
	if os.Getenv("SGATE_STRESS_SESSION") == "1" {
		for i := 0; i < 8; i++ {
			if err := <-results; err != nil {
				t.Fatal(err)
			}
		}
		for _, c := range conns {
			c.Close()
		}
		for i := 0; i < 8; i++ {
			<-results
		}
		return
	}
	for i := 0; i < 16; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
}
