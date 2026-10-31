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
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Verify the previously missing client-to-server reason over the real encrypted
// transport, including replay-capable replacement and isolation from healthy TCP.
func TestReverseFailureReasonWSWSS(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprint("WSS=", secure), func(t *testing.T) {
			bad, badListen, goodListen := reverseAddress(t), reverseAddress(t), reverseAddress(t)
			good := targetBackend(t, "H")
			var logs failureLogs
			opts := testOptions()
			opts.Logf = logs.logf
			s, err := NewServer(ServerConfig{Key: bytes.Repeat([]byte{73}, 32), Target: good, Options: opts,
				Accounts: []Account{{Username: "alice", Password: testPassword, ReverseMappings: []ReverseMapping{
					{Listen: badListen, Target: bad}, {Listen: goodListen, Target: good},
				}}}})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(s.Close)
			if err := s.StartReverse(); err != nil {
				t.Fatal(err)
			}
			web := httptest.NewUnstartedServer(s)
			if secure {
				web.StartTLS()
			} else {
				web.Start()
			}
			t.Cleanup(web.Close)
			link := connectReverse(t, s, web, "alice", secure)
			healthy := dialReverse(t, goodListen)
			readMarker(t, healthy, "H")
			for attempt := 0; attempt < 2; attempt++ {
				assertSocketClosed(t, dialReverse(t, badListen))
				waitFor(t, "detailed reverse RESET", func() bool {
					return strings.Count(logs.text(), "backend TCP connection refused") == attempt+1
				})
				if err := exchange(healthy, []byte("healthy after refused target")); err != nil {
					t.Fatal(err)
				}
				if attempt == 0 {
					link.wsClose()
					<-link.done
					waitFor(t, "detach", func() bool { s.mu.Lock(); defer s.mu.Unlock(); return !s.sessions[link.id].inUse })
					link.attach(t, s, true)
				}
			}
			for _, want := range []string{"flow=reverse", fmt.Sprintf("stream_target=%q", bad), "event=stream_remote_reset reason_code=4"} {
				if !strings.Contains(logs.text(), want) {
					t.Fatalf("missing %q: %s", want, logs.text())
				}
			}
		})
	}
}

// An old server that omits confirmation must still receive empty RESETs. A
// forged or transport-replayed confirmation must fail before business traffic.
func TestClientStreamErrorConfirmation(t *testing.T) {
	for _, mode := range []string{"legacy", "confirmed", "forged", "replayed", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			s, err := NewServer(ServerConfig{Key: bytes.Repeat([]byte{74}, 32), Password: testPassword, Target: "127.0.0.1:1", Options: testOptions()})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			resets := make(chan frame, 1)
			web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				request, ok := s.authenticateRequest(r)
				if !ok {
					http.Error(w, "auth", 403)
					return
				}
				nonce := randomID()
				headers := s.handshakeHeaders(request, nonce)
				switch mode {
				case "legacy":
					headers.Del(streamErrorsHeader)
					headers.Del(streamErrorsAuthHeader)
				case "forged":
					headers.Set(streamErrorsAuthHeader, "forged")
				case "replayed":
					headers.Set(streamErrorsAuthHeader, streamErrorsMAC(s.cfg.Key, request.id, request.nonce, randomID()))
				case "unknown":
					headers.Set(streamErrorsHeader, "2")
				}
				ws, err := (&websocket.Upgrader{}).Upgrade(w, r, headers)
				if err != nil {
					return
				}
				defer ws.Close()
				_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
				crypt := newCodec(s.cfg.Key, request.id, request.nonce, nonce, false)
				for {
					_, wire, err := ws.ReadMessage()
					if err != nil {
						return
					}
					plain, err := crypt.open(wire)
					if err != nil {
						t.Error(err)
						return
					}
					f, err := decodeFrame(plain)
					if err != nil {
						t.Error(err)
						return
					}
					if f.op == opReset {
						resets <- f
						return
					}
				}
			}))
			defer web.Close()
			client, err := NewClient(ClientConfig{Key: s.cfg.Key, Password: testPassword, URL: "ws" + strings.TrimPrefix(web.URL, "http") + "/tunnel", Options: testOptions()})
			if err != nil {
				t.Fatal(err)
			}
			ws, crypt, _, err := client.dial(context.Background(), randomID(), false)
			if mode != "legacy" && mode != "confirmed" {
				if err == nil {
					ws.Close()
					t.Fatal("accepted untrusted capability")
				}
				if !strings.Contains(err.Error(), "capability authentication failed") {
					t.Fatal(err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer ws.Close()
			local := newSession(context.Background(), "", client.cfg.Options)
			defer local.close()
			done := make(chan struct{})
			go func() { _ = local.transport(ws, crypt); close(done) }()
			defer func() { ws.Close(); <-done }()
			waitFor(t, "transport attached", func() bool { local.mu.Lock(); defer local.mu.Unlock(); return local.active != nil })
			local.enqueueResetReason(reverseStreamBase+1, resetRefused)
			select {
			case f := <-resets:
				if mode == "legacy" && len(f.data) != 0 {
					t.Fatal("extended RESET sent to legacy server")
				}
				if mode == "confirmed" && !bytes.Equal(f.data, []byte{resetRefused}) {
					t.Fatalf("missing reason: %+v", f)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("RESET not sent")
			}
		})
	}
}

func TestStreamFailureEventClassification(t *testing.T) {
	for _, tc := range []struct {
		operation string
		err       error
		event     string
	}{
		{"TCP write", &net.OpError{Op: "write", Err: syscall.EPIPE}, "stream_peer_closed"},
		{"TCP half-close", fmt.Errorf("TLS close: %w", syscall.ECONNRESET), "stream_peer_closed"},
		{"TCP half-close", syscall.ENOTCONN, "stream_peer_closed"},
		{"TCP write", errors.New("unexpected failure"), "stream_error"},
		{"TLS handshake", context.DeadlineExceeded, "ingress_tls_rejected"},
	} {
		if got := streamFailureEvent(tc.operation, tc.err); got != tc.event {
			t.Fatalf("%s %v: %s", tc.operation, tc.err, got)
		}
	}
}
