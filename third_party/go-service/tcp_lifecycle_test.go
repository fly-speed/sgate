package master

import (
	"context"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func waitTCP(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("TCP lifecycle condition timed out")
}
func tcpFixture(t *testing.T, callbacks ...CloseFunc) (*TcpService, *TCPWorker, <-chan struct{}) {
	t.Helper()
	s, err := TcpServiceInit("127.0.0.1:0", WithTCPContext(context.Background()), WithTCPManaged(false))
	if err != nil {
		t.Fatal(err)
	}
	s.AcceptHandler = s.AcceptConn
	if len(callbacks) != 0 {
		s.CloseHandler = callbacks[0]
	}
	w, err := s.Configure("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); s.Run() }()
	t.Cleanup(func() { s.Close(); <-done; waitTCP(t, func() bool { return ConnCountCur() == 0 }) })
	return s, w, done
}

func TestTCPCallbackLifetimeAndHalfClose(t *testing.T) {
	var closes atomic.Int32
	s, w, _ := tcpFixture(t, func(net.Conn) { closes.Add(1) })
	_ = s
	app, err := net.Dial("tcp", w.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	peer, err := w.Accept()
	if err != nil {
		t.Fatal(err)
	}
	waitTCP(t, func() bool { return ConnCountCur() == 1 })
	_, _ = app.Write([]byte("request"))
	_ = app.(*net.TCPConn).CloseWrite()
	got, err := io.ReadAll(peer)
	if err != nil || string(got) != "request" {
		t.Fatalf("%q %v", got, err)
	}
	_, _ = peer.Write([]byte("response"))
	if err = peer.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err = io.ReadAll(app)
	if err != nil || string(got) != "response" {
		t.Fatalf("%q %v", got, err)
	}
	peer.Close()
	waitTCP(t, func() bool { return ConnCountCur() == 0 && closes.Load() == 1 })
}

func TestTCPRebindFailureRollbackAndCancellation(t *testing.T) {
	s, w, done := tcpFixture(t)
	address := w.Addr().String()
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	w.Close()
	if _, err = s.Configure(occupied.Addr().String()); err == nil {
		t.Fatal("bound occupied port")
	}
	replacement, err := s.Configure(address)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	// No application Accept: shutdown must also join a blocked handoff.
	waitTCP(t, func() bool { return ConnCountCur() == 1 })
	s.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not exit")
	}
	replacement.Close()
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal("listener leaked", err)
	}
	listener.Close()
}

func TestTCPRepeatedWorkersReleaseCallbacks(t *testing.T) {
	s, w, _ := tcpFixture(t)
	for i := 0; i < 20; i++ {
		app, err := net.Dial("tcp", w.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		peer, err := w.Accept()
		if err != nil {
			t.Fatal(err)
		}
		w.Close()
		app.Close()
		peer.Close()
		waitTCP(t, func() bool { return ConnCountCur() == 0 })
		w, err = s.Configure("127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
	}
	w.Close()
}
