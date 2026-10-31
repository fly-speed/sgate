package tunnel

import (
	"context"
	"fmt"
	"github.com/gorilla/websocket"
	"net"
	"strings"
	"testing"
	"time"
)

func TestReversePermissionDefaultAndURLCannotBypass(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprintf("TLS=%t", secure), func(t *testing.T) {
			opts := testOptions()
			s, web, address := reverseHarness(t, secure, targetBackend(t, "P"), opts)
			// Reuse only connection settings from an explicitly allowed client. A new
			// zero-value permission must override even copied private session options.
			allowed := connectReverseOptions(t, s, web, "alice", secure, opts)
			cfg := allowed.client.cfg
			allowed.local.close()
			allowed.wsClose()
			<-allowed.done
			waitFor(t, "allowed transport detached", func() bool {
				s.mu.Lock()
				defer s.mu.Unlock()
				return s.sessions[allowed.id] == nil || !s.sessions[allowed.id].inUse
			})
			cfg.AllowReverse = false
			cfg.URL += "?" + reverseQuery + "=1&" + reverseQuery + "=1&" + reverseTLSQuery + "=1"
			client, err := NewClient(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if client.cfg.Options.receiveReverse {
				t.Fatal("disabled permission retained copied reverse capability")
			}
			id := randomID()
			ws, _, _, err := client.dial(context.Background(), id, false)
			if err != nil {
				t.Fatal(err)
			}
			defer ws.Close()
			s.mu.Lock()
			advertised := s.sessions[id].reverse
			s.mu.Unlock()
			if advertised {
				t.Fatal("URL query bypassed local reverse permission")
			}
			assertSocketClosed(t, dialReverse(t, address))
			// A trusted/authenticated server must still be unable to force a local dial.
			backend, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer backend.Close()
			local := newSession(context.Background(), "", client.cfg.Options)
			defer local.close()
			if err := local.apply(frame{op: opReverseOpen, stream: reverseStreamBase + 1, data: []byte(backend.Addr().String())}); err == nil {
				t.Fatal("disabled client accepted server reverse command")
			}
			backend.(*net.TCPListener).SetDeadline(time.Now().Add(30 * time.Millisecond))
			if conn, err := backend.Accept(); err == nil {
				conn.Close()
				t.Fatal("disabled reverse command dialed target")
			} else if e, ok := err.(net.Error); !ok || !e.Timeout() {
				t.Fatal(err)
			}
		})
	}
}

func TestReversePermissionRequiresExplicitOptInAndIdentity(t *testing.T) {
	for _, test := range []struct {
		name, username string
		allow, want    bool
	}{
		{name: "old config", username: "alice"},
		{name: "opted in", username: "alice", allow: true, want: true},
		{name: "anonymous cannot reverse", allow: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, err := NewClient(ClientConfig{Key: []byte(strings.Repeat("k", 32)), Password: testPassword, Username: test.username, AllowReverse: test.allow, URL: "ws://localhost:8080/tunnel", Options: testOptions()})
			if err != nil {
				t.Fatal(err)
			}
			if client.cfg.Options.receiveReverse != test.want {
				t.Fatal("incorrect local reverse capability")
			}
		})
	}
}

func TestReversePermissionCloseProofCannotBeForgedOrReplayed(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprintf("TLS=%t", secure), func(t *testing.T) {
			noiseRecoveryResourceCheck(t)
			s, web, address := reverseHarness(t, secure, targetBackend(t, "C"), testOptions())
			link := connectReverse(t, s, web, "alice", secure)
			remote := reverseRemote(t, s, link)
			conn := dialReverse(t, address)
			readMarker(t, conn, "C")
			link.local.mu.Lock()
			oldProof := link.local.activeCloseProof
			link.local.mu.Unlock()
			if oldProof == "" {
				t.Fatal("opted-in transport lacks close proof")
			}
			for _, proof := range []string{strings.Repeat("0", 64), oldProof} {
				link.local.mu.Lock()
				ws := link.local.active
				link.local.mu.Unlock()
				if err := ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, sessionClosePrefix+proof), time.Now().Add(time.Second)); err != nil {
					t.Fatal(err)
				}
				select {
				case <-link.done:
				case <-time.After(3 * time.Second):
					t.Fatal("untrusted Close did not end transport")
				}
				waitFor(t, "transport detached after untrusted close", func() bool {
					s.mu.Lock()
					defer s.mu.Unlock()
					return s.sessions[link.id] != nil && !s.sessions[link.id].inUse
				})
				remote.mu.Lock()
				closed := remote.closed
				remote.mu.Unlock()
				if closed {
					t.Fatal("untrusted Close destroyed resumable state")
				}
				link.attach(t, s, true)
				if reverseRemote(t, s, link) != remote {
					t.Fatal("untrusted Close replaced original session")
				}
				if err := exchange(conn, []byte("retained-after-untrusted-close")); err != nil {
					t.Fatal(err)
				}
			}
			// A deliberate local stop sends the current proof, unlike a network outage.
			link.local.close()
			assertSocketClosed(t, conn)
			assertReleased(t, "deliberately closed reverse session", remote)
			waitFor(t, "explicit close removed session admission", func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.sessions[link.id] == nil })
		})
	}
}
