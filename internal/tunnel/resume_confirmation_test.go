package tunnel

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Model write backpressure for original-deadline and Ping/ACK regressions.
type resumeBlockedConn struct {
	net.Conn
	blocked   atomic.Bool
	entered   chan struct{}
	closed    chan struct{}
	release   chan struct{} // Optional controlled progress for backpressure tests.
	writeOnce sync.Once
	closeOnce sync.Once
}

func (c *resumeBlockedConn) Write(p []byte) (int, error) {
	if c.blocked.Load() {
		c.writeOnce.Do(func() { close(c.entered) })
		select {
		case <-c.closed:
			return 0, net.ErrClosed
		case <-c.release:
		}
	}
	return c.Conn.Write(p)
}
func (c *resumeBlockedConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

type resumeBlockedListener struct {
	net.Listener
	accepted chan *resumeBlockedConn
}

func (l resumeBlockedListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	b := &resumeBlockedConn{Conn: c, entered: make(chan struct{}), closed: make(chan struct{})}
	l.accepted <- b
	return b, nil
}

func TestUnconfirmedDeadlineInterruptsBlockedControlWrite(t *testing.T) {
	key := bytes.Repeat([]byte{42}, 32)
	opts := testOptions()
	opts.WriteTimeout, opts.Heartbeat = 5*time.Second, time.Hour
	opts, err := opts.defaults()
	if err != nil {
		t.Fatal(err)
	}
	s := newSession(context.Background(), "", opts)
	defer s.close()
	if err := s.enqueue(opData, 1, []byte("pending")); err != nil {
		t.Fatal(err)
	}
	accepted := make(chan *resumeBlockedConn, 1)
	done := make(chan error, 1)
	var deadline time.Time
	h := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			done <- err
			return
		}
		ws.UnderlyingConn().(*resumeBlockedConn).blocked.Store(true)
		deadline = time.Now().Add(200 * time.Millisecond)
		done <- s.transportConfirmed(ws, newCodec(key, "id", "client", "server", false), deadline, nil)
	}))
	h.Listener = resumeBlockedListener{h.Listener, accepted}
	h.Start()
	defer h.Close()
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	c := <-accepted
	select {
	case <-c.entered:
	case <-time.After(time.Second):
		t.Fatal("transport write did not block")
	}
	if err := ws.WriteControl(websocket.PingMessage, []byte("probe"), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		if time.Now().Before(deadline) {
			t.Fatal("unconfirmed transport closed early")
		}
	case <-time.After(time.Second):
		c.Close()
		<-done
		t.Fatal("read deadline did not interrupt blocked control write at original expiry")
	}
}

func TestResumeConfirmationControlsRetention(t *testing.T) {
	for _, mode := range []string{"abandoned", "confirmed", "silent", "control", "fragmented"} {
		t.Run(mode, func(t *testing.T) {
			key := bytes.Repeat([]byte{42}, 32)
			s, err := NewServer(ServerConfig{
				Key:        key,
				Password:   testPassword,
				Target:     "127.0.0.1:1",
				SessionTTL: time.Second,
				Options:    testOptions(),
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(s.Close)
			h := httptest.NewServer(s)
			t.Cleanup(h.Close)
			// Seed the same negotiated state that the current client creates.
			oldOptions := s.cfg.Options
			oldOptions.reportStreamErrors = true
			old := newSession(s.ctx, s.cfg.Target, oldOptions)
			if err := old.enqueue(opData, 1, []byte("retained")); err != nil {
				t.Fatal(err)
			}
			idle, id := time.Now().Add(-100*time.Millisecond), randomID()
			e := &serverEntry{session: old, idle: idle}
			s.mu.Lock()
			s.sessions[id] = e
			s.mu.Unlock()
			c, err := NewClient(ClientConfig{
				Key:      key,
				Password: testPassword,
				URL:      "ws" + strings.TrimPrefix(h.URL, "http") + "/tunnel",
				Options:  testOptions(),
			})
			if err != nil {
				t.Fatal(err)
			}
			ws, codec, _, err := c.dial(context.Background(), id, true)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { ws.Close() })
			if mode == "control" || mode == "fragmented" {
				stop, readerDone, writerDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
				var writeMu sync.Mutex
				var sent atomic.Int64
				ws.SetPingHandler(func(data string) error {
					writeMu.Lock()
					defer writeMu.Unlock()
					return ws.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(time.Second))
				})
				go func() {
					defer close(readerDone)
					for {
						if _, _, err := ws.ReadMessage(); err != nil {
							return
						}
					}
				}()
				go func() {
					defer close(writerDone)
					ticker := time.NewTicker(25 * time.Millisecond)
					defer ticker.Stop()
					payload := codec.seal((frame{op: opNoise, data: make([]byte, 8192)}).encode())
					offset := 0
					for {
						select {
						case <-stop:
							return
						case <-ticker.C:
						}
						writeMu.Lock()
						var err error
						if mode == "control" {
							err = ws.WriteControl(websocket.PingMessage, []byte("probe"), time.Now().Add(time.Second))
							if err == nil {
								err = ws.WriteControl(websocket.PongMessage, []byte("probe"), time.Now().Add(time.Second))
							}
						} else {
							op := byte(0)
							if offset == 0 {
								op = websocket.BinaryMessage
							}
							_ = ws.UnderlyingConn().SetWriteDeadline(time.Now().Add(time.Second))
							err = writeUnconfirmedFragment(ws.UnderlyingConn(), op, payload[offset:offset+8])
							offset += 8
						}
						writeMu.Unlock()
						if err != nil {
							return
						}
						sent.Add(1)
					}
				}()
				defer func() {
					close(stop)
					ws.Close()
					for _, done := range []chan struct{}{readerDone, writerDone} {
						select {
						case <-done:
						case <-time.After(time.Second):
							t.Error("unconfirmed probe worker leaked")
						}
					}
					if sent.Load() < 10 {
						t.Error("insufficient unconfirmed progress")
					}
				}()
				stressWaitForBudget(
					t,
					"active unconfirmed resume expires at original deadline",
					1500*time.Millisecond,
					func() bool { return sessionReleased(old) },
				)
				s.mu.Lock()
				after := e.idle
				s.mu.Unlock()
				if !after.Equal(idle) {
					t.Fatal("unconfirmed progress renewed retention")
				}
				return
			}
			if mode == "silent" {
				stressWaitForBudget(
					t,
					"unconfirmed resume expires at original deadline",
					1500*time.Millisecond,
					func() bool { return sessionReleased(old) },
				)
				return
			}
			if mode == "confirmed" {
				if err := ws.WriteMessage(websocket.BinaryMessage, codec.seal((frame{op: opAck, ack: 1}).encode())); err != nil {
					t.Fatal(err)
				}
				waitFor(
					t,
					"confirmed peer acknowledgement",
					func() bool {
						old.mu.Lock()
						defer old.mu.Unlock()
						return old.pendingBytes == 0
					},
				)
				// Confirmation must disarm the original deadline, including when
				// its timer callback races a successfully authenticated frame.
				time.Sleep(max(time.Until(idle.Add(s.cfg.SessionTTL)), 0) + 100*time.Millisecond)
				if err := ws.WriteMessage(websocket.BinaryMessage, codec.seal((frame{op: opNoise, data: []byte{1}}).encode())); err != nil {
					t.Fatal(err)
				}
				waitFor(
					t,
					"confirmed resume survives original deadline",
					func() bool {
						old.mu.Lock()
						defer old.mu.Unlock()
						return old.noiseReceived > 0
					},
				)
			}
			ws.Close()
			waitFor(t, "resume handler returned", func() bool {
				s.mu.Lock()
				defer s.mu.Unlock()
				return !e.inUse
			})
			s.mu.Lock()
			after := e.idle
			s.mu.Unlock()
			if mode == "abandoned" && !after.Equal(idle) {
				t.Fatal("abandoned resume renewed old session retention")
			}
			if mode == "confirmed" && !after.After(idle) {
				t.Fatal("confirmed resume did not start a fresh retention window")
			}
		})
	}
}
