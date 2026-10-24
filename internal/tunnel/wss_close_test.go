package tunnel

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Shared management cannot close its own HTTP worker to unblock a tunnel.
// Session shutdown must itself close the raw TLS transport before TLS.Close
// waits for a writer whose peer has stopped reading.
func TestSessionCloseUnblocksTLSWriter(t *testing.T) {
	accepted := make(chan *websocket.Conn, 1)
	release := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, e := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if e != nil {
			return
		}
		defer ws.Close()
		accepted <- ws
		<-release
	}))
	defer server.Close()
	defer close(release)
	dialer := websocket.Dialer{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} // isolated test certificate
	peer, _, e := dialer.Dial("wss"+strings.TrimPrefix(server.URL, "https"), nil)
	if e != nil {
		t.Fatal(e)
	}
	defer peer.Close()
	var ws *websocket.Conn
	select {
	case ws = <-accepted:
	case <-time.After(time.Second):
		t.Fatal("upgrade not accepted")
	}
	raw := ws.NetConn().(*tls.Conn).NetConn()
	defer raw.Close()
	if tcp, ok := raw.(*net.TCPConn); ok {
		_ = tcp.SetWriteBuffer(1024)
	}
	if tcp, ok := peer.NetConn().(*tls.Conn).NetConn().(*net.TCPConn); ok {
		_ = tcp.SetReadBuffer(1024)
	}
	s := newSession(context.Background(), "", testOptions())
	s.mu.Lock()
	s.active = ws
	s.mu.Unlock()
	defer s.close()
	written := make(chan struct{})
	go func() {
		defer close(written)
		_, _ = ws.NetConn().Write(make([]byte, 16<<20))
	}()
	select {
	case <-written:
		t.Fatal("expected blocked TLS writer")
	case <-time.After(100 * time.Millisecond):
	}
	closed := make(chan struct{})
	go func() {
		s.close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		_ = raw.Close()
		t.Fatal("session close waited for blocked TLS writer")
	}
	select {
	case <-written:
	case <-time.After(time.Second):
		t.Fatal("TLS writer retained after session close")
	}
}
