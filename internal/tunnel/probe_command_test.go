package tunnel

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type commandProbeFixture struct {
	server                 *Server
	client                 *Client
	accepted               atomic.Int32
	serverLogs, clientLogs failureLogs
}

func newCommandProbeFixture(t *testing.T, secure bool) *commandProbeFixture {
	t.Helper()
	f := &commandProbeFixture{}
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { backend.Close() })
	go func() {
		for {
			conn, err := backend.Accept()
			if err != nil {
				return
			}
			f.accepted.Add(1)
			io.Copy(io.Discard, conn)
			conn.Close()
		}
	}()
	opts := testOptions()
	opts.DialTimeout = 250 * time.Millisecond
	opts.Logf = f.serverLogs.logf
	f.server, err = NewServer(ServerConfig{Key: bytes.Repeat([]byte{68}, 32), Password: testPassword, Target: backend.Addr().String(), Options: opts})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.server.Close)
	var web *httptest.Server
	if secure {
		web = httptest.NewTLSServer(f.server)
	} else {
		web = httptest.NewServer(f.server)
	}
	t.Cleanup(web.Close)
	opts.Logf = f.clientLogs.logf
	cfg := ClientConfig{Key: f.server.cfg.Key, Password: testPassword, URL: "ws" + strings.TrimPrefix(web.URL, "http") + "/tunnel", Options: opts}
	if secure {
		roots := x509.NewCertPool()
		roots.AddCert(web.Certificate())
		cfg.TLSConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	}
	f.client, err = NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *commandProbeFixture) dial(t *testing.T) (*websocket.Conn, *codec, string) {
	t.Helper()
	client := *f.client
	client.cfg.URL += "?" + probeQuery + "=1"
	id := randomID()
	ws, crypt, _, err := client.dial(context.Background(), id, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	_ = ws.SetReadDeadline(time.Now().Add(time.Second))
	return ws, crypt, id
}

func TestEncryptedTestCommandRequiredBeforeBackendDial(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprint(secure), func(t *testing.T) {
			f := newCommandProbeFixture(t, secure)
			ws, crypt, id := f.dial(t)
			if f.accepted.Load() != 0 {
				t.Fatal("backend dialed during HTTP handshake")
			}
			plain := []byte(fmt.Sprintf(`{"command":"TEST_CONNECT","test_id":%q}`, id))
			sealed := crypt.seal(plain)
			if bytes.Contains(sealed, []byte("TEST_CONNECT")) {
				t.Fatal("test command exposed as plaintext")
			}
			if err := ws.WriteMessage(websocket.BinaryMessage, sealed); err != nil {
				t.Fatal(err)
			}
			var reply probeReply
			if err := readProbeMessage(ws, crypt, &reply); err != nil {
				t.Fatal(err)
			}
			if reply.Command != probeResultCommand || reply.TestID != id || reply.Status != "ok" || reply.Reason != 0 {
				t.Fatalf("reply: %+v", reply)
			}
			ws.Close()
			waitFor(t, "one backend dial and released probe", func() bool { return f.accepted.Load() == 1 && len(f.server.probeGate) == 0 })
			if !strings.Contains(f.serverLogs.text(), id+" command="+probeConnectCommand+" received") {
				t.Fatal("missing correlated server command log", f.serverLogs.text())
			}
			result := f.client.Probe(context.Background())
			if !result.OK || !validID(result.TestID) {
				t.Fatal(result)
			}
			waitFor(t, "correlated reply log", func() bool {
				return strings.Contains(f.serverLogs.text(), result.TestID+" command="+probeResultCommand+" sent")
			})
			if !strings.Contains(f.clientLogs.text(), result.TestID) || !strings.Contains(f.clientLogs.text(), probeConnectCommand+" completed") {
				t.Fatal("client log lacks correlation", f.clientLogs.text())
			}
		})
	}
}

func TestInvalidTestCommandsDoNotDialBackend(t *testing.T) {
	for _, kind := range []string{"unknown", "wrong-id", "plaintext", "tampered", "oversized", "silent", "fragmented", "duplicate"} {
		t.Run(kind, func(t *testing.T) {
			f := newCommandProbeFixture(t, false)
			ws, crypt, id := f.dial(t)
			command := probeCommand{Command: probeConnectCommand, TestID: id}
			switch kind {
			case "unknown":
				command.Command = "OPEN"
				_ = writeProbeMessage(ws, crypt, command)
			case "wrong-id":
				command.TestID = randomID()
				_ = writeProbeMessage(ws, crypt, command)
			case "plaintext":
				_ = ws.WriteMessage(websocket.BinaryMessage, []byte(`{"command":"TEST_CONNECT"}`))
			case "tampered":
				data := crypt.seal([]byte(`{}`))
				data[len(data)-1] ^= 1
				_ = ws.WriteMessage(websocket.BinaryMessage, data)
			case "oversized":
				_ = ws.WriteMessage(websocket.BinaryMessage, bytes.Repeat([]byte{1}, maxProbeMessageBytes+1))
			case "fragmented":
				data := crypt.seal([]byte(fmt.Sprintf(`{"command":"TEST_CONNECT","test_id":%q}`, id)))
				_ = writeUnconfirmedFragment(ws.NetConn(), websocket.BinaryMessage, data[:8])
			case "duplicate":
				data := crypt.seal([]byte(fmt.Sprintf(`{"command":"TEST_CONNECT","test_id":%q}`, id)))
				_ = ws.WriteMessage(websocket.BinaryMessage, data)
				_ = ws.WriteMessage(websocket.BinaryMessage, data)
			}
			_, _, err := ws.ReadMessage()
			if kind != "duplicate" && err == nil {
				t.Fatal("invalid command accepted")
			}
			ws.Close()
			waitFor(t, "probe released", func() bool { return len(f.server.probeGate) == 0 })
			if kind == "duplicate" {
				waitFor(t, "single backend dial", func() bool { return f.accepted.Load() == 1 })
			} else if f.accepted.Load() != 0 {
				t.Fatal("invalid command dialed backend")
			}
			f.server.mu.Lock()
			defer f.server.mu.Unlock()
			if len(f.server.sessions) != 0 {
				t.Fatal("test retained a business session")
			}
		})
	}
}

func TestProbeCommandShutdownReleasesSilentConnection(t *testing.T) {
	f := newCommandProbeFixture(t, true)
	ws, _, _ := f.dial(t)
	started := time.Now()
	f.server.Close()
	if _, _, err := ws.ReadMessage(); err == nil {
		t.Fatal("shutdown did not close silent test")
	}
	waitFor(t, "shutdown released probe permit", func() bool { return len(f.server.probeGate) == 0 })
	if time.Since(started) > time.Second || f.accepted.Load() != 0 {
		t.Fatal("shutdown retained test connection or dialed backend")
	}
}

// A valid HTTP/WS handshake is insufficient: success requires the right
// encrypted result command, matching ID, and an explicit consistent status.
func TestProbeResultValidation(t *testing.T) {
	key := bytes.Repeat([]byte{69}, 32)
	for _, kind := range []string{"wrong-id", "wrong-command", "missing-status", "invalid-reason", "inconsistent", "plaintext", "old-protocol"} {
		t.Run(kind, func(t *testing.T) {
			web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				request := tunnelRequest{id: r.Header.Get("X-Sgate-Session"), nonce: r.Header.Get("X-Sgate-Nonce"), timestamp: r.Header.Get("X-Sgate-Time"), credential: r.Header.Get(passwordHeader)}
				nonce := randomID()
				s := &Server{cfg: ServerConfig{Key: key}}
				headers := s.handshakeHeaders(request, nonce)
				version := probeCommandProtocol
				if kind == "old-protocol" {
					version = "ok"
				}
				headers.Set("X-Sgate-Probe", version)
				headers.Set("X-Sgate-Probe-Auth", probeMAC(key, request, nonce, version))
				ws, err := (&websocket.Upgrader{}).Upgrade(w, r, headers)
				if err != nil {
					return
				}
				defer ws.Close()
				if kind == "old-protocol" {
					return
				}
				crypt := newCodec(key, request.id, request.nonce, nonce, false)
				var command probeCommand
				if readProbeMessage(ws, crypt, &command) != nil {
					return
				}
				reply := probeReply{Command: probeResultCommand, TestID: command.TestID, Status: "ok"}
				switch kind {
				case "wrong-id":
					reply.TestID = randomID()
				case "wrong-command":
					reply.Command = "PONG"
				case "missing-status":
					reply.Status = ""
				case "invalid-reason":
					reply.Status = "failed"
					reply.Reason = 255
				case "inconsistent":
					reply.Reason = resetRefused
				case "plaintext":
					_ = ws.WriteMessage(websocket.BinaryMessage, []byte(`{}`))
					return
				}
				_ = writeProbeMessage(ws, crypt, reply)
			}))
			defer web.Close()
			client, err := NewClient(ClientConfig{Key: key, Password: testPassword, URL: "ws" + strings.TrimPrefix(web.URL, "http") + "/tunnel", Options: testOptions()})
			if err != nil {
				t.Fatal(err)
			}
			result := client.Probe(context.Background())
			if result.OK {
				t.Fatal("invalid result reported success", result)
			}
			if kind != "old-protocol" && result.Stage != "command" {
				t.Fatal("command failure incorrectly classified", result)
			}
		})
	}
}
