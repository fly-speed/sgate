package tunnel

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"

	"github.com/gorilla/websocket"
	"sync"
	"syscall"
	"testing"
	"time"
)

type failureLogs struct {
	mu    sync.Mutex
	lines []string
}

func (l *failureLogs) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}
func (l *failureLogs) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// Exercise the real TCP admission and encrypted WS/WSS control path. A failed
// mapping must report its own listener/target without polluting a healthy one.
func TestMappingFailureLogsWSWSS(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprintf("TLS=%t", secure), func(t *testing.T) {
			closed, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			bad := closed.Addr().String()
			closed.Close()
			good := targetBackend(t, "H")
			var serverLog failureLogs
			opts := testOptions()
			opts.Logf = serverLog.logf
			opts.NoiseIntervalMin, opts.NoiseIntervalMax = time.Millisecond, 3*time.Millisecond
			server, err := NewServer(ServerConfig{Key: bytes.Repeat([]byte{61}, 32), Password: testPassword,
				Target: good, AllowedTargets: []string{bad}, Options: opts})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(server.Close)
			var web *httptest.Server
			if secure {
				web = httptest.NewTLSServer(server)
			} else {
				web = httptest.NewServer(server)
			}
			t.Cleanup(web.Close)
			base := "ws" + strings.TrimPrefix(web.URL, "http") + "/tunnel"
			clientFor := func(endpoint, password string, logs *failureLogs) *Client {
				options := opts
				options.Logf = logs.logf
				cfg := ClientConfig{Key: server.cfg.Key, Password: password, URL: endpoint, Options: options}
				if secure {
					roots := x509.NewCertPool()
					roots.AddCert(web.Certificate())
					cfg.TLSConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
				}
				client, err := NewClient(cfg)
				if err != nil {
					t.Fatal(err)
				}
				return client
			}
			start := func(client *Client) (net.Listener, *clientSessions, func()) {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				state := &clientSessions{}
				done := make(chan error, 1)
				go func() { done <- client.run(ctx, listener, state) }()
				var once sync.Once
				stop := func() {
					once.Do(func() {
						cancel()
						select {
						case err := <-done:
							if err != nil {
								t.Error(err)
							}
						case <-time.After(5 * time.Second):
							t.Error("client did not stop")
						}
					})
				}
				t.Cleanup(stop)
				return listener, state, stop
			}
			var badLog, goodLog failureLogs
			badListener, state, stopBad := start(clientFor(base+"?addr="+url.QueryEscape(bad)+"&token=do-not-log", testPassword, &badLog))
			goodListener, _, stopGood := start(clientFor(base, testPassword, &goodLog))
			conn, err := net.Dial("tcp", badListener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(5 * time.Second))
			conn.Write([]byte("failed mapping"))
			var b [1]byte
			if n, err := conn.Read(b[:]); n != 0 || err == nil {
				t.Fatalf("failure leaked application bytes: %d %v", n, err)
			}
			waitFor(t, "both failure logs", func() bool {
				return strings.Contains(badLog.text(), "backend TCP connection refused") && strings.Contains(serverLog.text(), "backend connection failed")
			})
			for _, want := range []string{badListener.Addr().String(), bad, "stream=1"} {
				if !strings.Contains(badLog.text(), want) {
					t.Fatalf("missing %q in client log: %s", want, badLog.text())
				}
			}
			waitFor(t, "failed stream and replay buffers released while tunnel remains online", func() bool {
				state.mu.Lock()
				local := state.current
				state.mu.Unlock()
				local.mu.Lock()
				clean := !local.closed && len(local.streams) == 0 && local.pendingBytes == 0 && local.incomingBytes == 0
				local.mu.Unlock()
				server.mu.Lock()
				defer server.mu.Unlock()
				for _, entry := range server.sessions {
					if entry.session.target == bad {
						entry.session.mu.Lock()
						clean = clean && entry.inUse && !entry.session.closed && len(entry.session.streams) == 0 && entry.session.pendingBytes == 0 && entry.session.incomingBytes == 0
						entry.session.mu.Unlock()
						return clean
					}
				}
				return false
			})
			if strings.Count(badLog.text(), "connection rejected/reset by remote") != 1 {
				t.Fatal("duplicate failure log", badLog.text())
			}
			if strings.Contains(badLog.text()+serverLog.text(), "do-not-log") || strings.Contains(badLog.text()+serverLog.text(), testPassword) {
				t.Fatal("credential/query leaked")
			}
			healthy, err := net.Dial("tcp", goodListener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer healthy.Close()
			healthy.SetDeadline(time.Now().Add(5 * time.Second))
			if _, err := io.ReadFull(healthy, b[:]); err != nil || b[0] != 'H' {
				t.Fatalf("wrong backend %q %v", b, err)
			}
			if err := exchange(healthy, bytes.Repeat([]byte("healthy-payload"), 8192)); err != nil {
				t.Fatal(err)
			}
			var rejected failureLogs
			client := clientFor(base+"?addr=127.0.0.1:1", testPassword, &rejected)
			ws, _, status, err := client.dial(context.Background(), randomID(), false)
			if ws != nil {
				ws.Close()
			}
			if status != 403 || err == nil || !strings.Contains(err.Error(), "backend target is not allowed") || !strings.Contains(serverLog.text(), "target not in allowed-targets") {
				t.Fatalf("missing allowlist failure: %d %v %s", status, err, serverLog.text())
			}
			client = clientFor(base, "wrong-password", &rejected)
			ws, _, status, err = client.dial(context.Background(), randomID(), false)
			if ws != nil {
				ws.Close()
			}
			if status != 404 || err == nil || !strings.Contains(err.Error(), "authentication rejected") || !strings.Contains(serverLog.text(), "invalid authentication") {
				t.Fatalf("missing authentication failure: %d %v", status, err)
			}
			for _, failure := range []struct {
				name, endpoint, password, want string
			}{
				{"allowlist", base + "?addr=127.0.0.1:1", testPassword, "backend target is not allowed"},
				{"authentication", base, "wrong-password", "authentication rejected"},
			} {
				var log failureLogs
				listener, _, stop := start(clientFor(failure.endpoint, failure.password, &log))
				app, err := net.Dial("tcp", listener.Addr().String())
				if err != nil {
					t.Fatal(err)
				}
				waitFor(t, failure.name+" mapping log", func() bool {
					return strings.Contains(log.text(), failure.want) && strings.Contains(log.text(), listener.Addr().String())
				})
				stop()
				app.Close()
			}
			stopBad()
			stopGood()
			state.mu.Lock()
			local := state.current
			state.mu.Unlock()
			local.mu.Lock()
			if !local.closed || len(local.streams) != 0 || local.pendingBytes != 0 || local.incomingBytes != 0 {
				t.Error("failed mapping retained resources")
			}
			local.mu.Unlock()
			server.Close()
			server.mu.Lock()
			if len(server.sessions) != 0 {
				t.Error("server retained sessions")
			}
			server.mu.Unlock()
		})
	}
}

func TestStreamFailureReasonsAndLegacy(t *testing.T) {
	for _, negotiated := range []bool{false, true} {
		opts, _ := testOptions().defaults()
		opts.reportStreamErrors = negotiated
		opts.MaxStreams = 1
		var logs failureLogs
		opts.Logf = logs.logf
		s := newSession(context.Background(), "127.0.0.1:1", opts)
		s.mu.Lock()
		s.lastOpened = 1
		s.streams[1] = &stream{}
		s.mu.Unlock()
		// Use a full stream table to avoid network dependency.
		if err := s.apply(frame{op: opOpen, seq: 1, stream: 2}); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "reset frame", func() bool { s.mu.Lock(); defer s.mu.Unlock(); return len(s.pending) == 1 })
		s.mu.Lock()
		f := s.pending[0]
		delete(s.streams, 1)
		s.mu.Unlock()
		if _, err := decodeFrame(f.encode()); err != nil {
			t.Fatal(err)
		}
		if negotiated && !bytes.Equal(f.data, []byte{resetStreamLimit}) || !negotiated && len(f.data) != 0 {
			t.Fatalf("legacy compatibility: %+v", f)
		}
		s.close()
	}
	for _, data := range [][]byte{{0}, {255}, {1, 2}} {
		if _, err := decodeFrame((frame{op: opReset, seq: 1, stream: 1, data: data}).encode()); err == nil {
			t.Fatal("accepted invalid reset reason", data)
		}
	}
	for _, tc := range []struct {
		err    error
		reason byte
	}{
		{&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, resetRefused},
		{&net.DNSError{Err: "no such host"}, resetDNSFailed},
		{&net.OpError{Op: "dial", Err: context.DeadlineExceeded}, resetDialTimeout},
		{syscall.ENETUNREACH, resetUnreachable}, {errors.New("other"), resetDialFailed},
	} {
		if got := dialResetReason(tc.err); got != tc.reason {
			t.Fatalf("classification: %v -> %d", tc.err, got)
		}
	}
}

func TestFailureLogSanitization(t *testing.T) {
	var logs failureLogs
	logger := mappingLogger(logs.logf, "127.0.0.1:9001", "ws://user:secret@localhost:8081/tunnel?addr=127.0.0.1%3A8080&token=private")
	logger("connection failed: %v", safeDialError(&url.Error{Op: "Get", URL: "http://secret", Err: syscall.ECONNREFUSED}))
	for _, secret := range []string{"secret", "private", "user:"} {
		if strings.Contains(logs.text(), secret) {
			t.Fatal("leaked URL", logs.text())
		}
	}
	if !strings.Contains(logs.text(), "connection refused") {
		t.Fatal(logs.text())
	}
}

func TestDiagnosticCapabilityCannotChangeOnResume(t *testing.T) {
	server, err := NewServer(ServerConfig{Key: bytes.Repeat([]byte{62}, 32), Password: testPassword, Target: "127.0.0.1:1", Options: testOptions()})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	id := randomID()
	request := tunnelRequest{id: id, nonce: randomID(), target: server.cfg.Target, reportStreamErrors: true}
	_, _, ok := server.acquireSession(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/tunnel", nil), request)
	if !ok {
		t.Fatal("initial acquire failed")
	}
	server.mu.Lock()
	server.sessions[id].inUse = false
	idle := server.sessions[id].idle
	server.mu.Unlock()
	request.nonce, request.resume, request.reportStreamErrors = randomID(), "1", false
	response := httptest.NewRecorder()
	_, _, ok = server.acquireSession(response, httptest.NewRequest(http.MethodGet, "/tunnel", nil), request)
	if ok || response.Code != http.StatusConflict {
		t.Fatal("diagnostic capability changed on resume")
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if !server.sessions[id].idle.Equal(idle) || server.sessions[id].inUse {
		t.Fatal("failed resume renewed session")
	}
}

// Legacy clients sign requests without the new capability query. They must
// continue to receive empty RESETs, including replay after a transport failure.
func TestLegacyFailureResetReplay(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	target := backend.Addr().String()
	backend.Close()
	server, err := NewServer(ServerConfig{Key: bytes.Repeat([]byte{63}, 32), Password: testPassword, Target: target, Options: testOptions()})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	web := httptest.NewServer(server)
	defer web.Close()
	id := randomID()
	endpoint := "ws" + strings.TrimPrefix(web.URL, "http") + "/tunnel"
	dial := func(resume string) (*websocket.Conn, *codec) {
		t.Helper()
		nonce, ts := randomID(), strconv.FormatInt(time.Now().Unix(), 10)
		headers, err := requestHeaders(server.cfg.Key, testPassword, id, nonce, ts, resume, "/tunnel")
		if err != nil {
			t.Fatal(err)
		}
		ws, response, err := websocket.DefaultDialer.Dial(endpoint, headers)
		if err != nil {
			t.Fatal(err)
		}
		ws.SetReadDeadline(time.Now().Add(5 * time.Second))
		return ws, newCodec(server.cfg.Key, id, nonce, response.Header.Get("X-Sgate-Nonce"), true)
	}
	readReset := func(ws *websocket.Conn, crypt *codec) frame {
		t.Helper()
		for {
			_, data, err := ws.ReadMessage()
			if err != nil {
				t.Fatal(err)
			}
			plain, err := crypt.open(data)
			if err != nil {
				t.Fatal(err)
			}
			f, err := decodeFrame(plain)
			if err != nil {
				t.Fatal(err)
			}
			if f.op == opReset {
				return f
			}
		}
	}
	ws, crypt := dial("0")
	if err := ws.WriteMessage(websocket.BinaryMessage, crypt.seal((frame{op: opOpen, seq: 1, stream: 1}).encode())); err != nil {
		t.Fatal(err)
	}
	original := readReset(ws, crypt)
	if len(original.data) != 0 {
		t.Fatal("sent extended RESET to legacy client")
	}
	ws.Close()
	waitFor(t, "legacy detach", func() bool { server.mu.Lock(); defer server.mu.Unlock(); return !server.sessions[id].inUse })
	ws, crypt = dial("1")
	defer ws.Close()
	// Confirmation is required before a retained session starts replaying.
	if err := ws.WriteMessage(websocket.BinaryMessage, crypt.seal((frame{op: opAck}).encode())); err != nil {
		t.Fatal(err)
	}
	replayed := readReset(ws, crypt)
	if original.seq != replayed.seq || original.stream != replayed.stream || len(replayed.data) != 0 {
		t.Fatalf("reset replay changed: %+v %+v", original, replayed)
	}
}

func TestFailureResetReplayLogsOnce(t *testing.T) {
	opts, _ := testOptions().defaults()
	var logs failureLogs
	opts.Logf = logs.logf
	s := newSession(context.Background(), "", opts)
	defer s.close()
	pipe, app := net.Pipe()
	defer app.Close()
	if err := s.addLocal(pipe); err != nil {
		t.Fatal(err)
	}
	f := frame{op: opReset, seq: 1, stream: 1, data: []byte{resetRefused}}
	stopped := make(chan struct{})
	if err := s.queueIncoming(f, stopped); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "reset delivered", func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.received == 1 })
	if err := s.queueIncoming(f, stopped); err != nil {
		t.Fatal(err)
	}
	if strings.Count(logs.text(), "backend TCP connection refused") != 1 {
		t.Fatal("reset replay repeated failure log", logs.text())
	}
}
