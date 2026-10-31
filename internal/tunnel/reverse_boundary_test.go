package tunnel

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Use a real, parked WebSocket so admission can be driven exactly to a limit
// without a transport writer draining the queue between assertions.
func reverseParkedSession(t *testing.T, opts Options) *session {
	t.Helper()
	accepted := make(chan *websocket.Conn, 1)
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		accepted <- ws
	}))
	t.Cleanup(web.Close)
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(web.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	s := newSession(context.Background(), "localhost:80", opts)
	select {
	case s.active = <-accepted:
	case <-time.After(time.Second):
		t.Fatal("parked transport not accepted")
	}
	t.Cleanup(s.close)
	return s
}

func reverseReservePipe(t *testing.T, s *session, target string) error {
	t.Helper()
	conn, peer := net.Pipe()
	t.Cleanup(func() { conn.Close(); peer.Close() })
	return s.reserveReverse(conn, target)
}

func TestReverseAdmissionBoundaries(t *testing.T) {
	t.Run("stream limit and recovery", func(t *testing.T) {
		opts, _ := testOptions().defaults()
		opts.MaxStreams = 2
		s := reverseParkedSession(t, opts)
		for i := 0; i < 2; i++ {
			if err := reverseReservePipe(t, s, "localhost:80"); err != nil {
				t.Fatal(err)
			}
		}
		if err := reverseReservePipe(t, s, "localhost:80"); err == nil {
			t.Fatal("admitted beyond MaxStreams")
		}
		s.mu.Lock()
		next, pending := s.next, s.pendingBytes
		s.mu.Unlock()
		if next != 2 {
			t.Fatal("rejected admission allocated a sequence")
		}
		s.drop(reverseStreamBase + 1)
		if err := reverseReservePipe(t, s, "localhost:80"); err != nil {
			t.Fatal("capacity did not recover", err)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.streams) != 2 || s.pendingBytes <= pending {
			t.Fatal("replacement stream not admitted")
		}
	})
	t.Run("exact buffer capacity and ACK recovery", func(t *testing.T) {
		target := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 61) + ":65535"
		cost := frameHeader + len(target)
		count := (chunkSize + frameHeader + cost - 1) / cost
		opts, _ := testOptions().defaults()
		opts.BufferBytes = count * cost
		opts.MaxStreams = count + 1
		s := reverseParkedSession(t, opts)
		for i := 0; i < count; i++ {
			if err := reverseReservePipe(t, s, target); err != nil {
				t.Fatal(err)
			}
		}
		if err := reverseReservePipe(t, s, target); err == nil {
			t.Fatal("full replay queue admitted another OPEN")
		}
		s.mu.Lock()
		if s.pendingBytes != opts.BufferBytes || int(s.next) != count || int(s.nextStream) != count {
			t.Error("capacity rejection changed queue or IDs")
		}
		s.mu.Unlock()
		if err := s.acknowledge(uint64(count)); err != nil {
			t.Fatal(err)
		}
		if err := reverseReservePipe(t, s, target); err != nil {
			t.Fatal("ACK did not recover admission", err)
		}
	})
	t.Run("last stream ID", func(t *testing.T) {
		opts, _ := testOptions().defaults()
		s := reverseParkedSession(t, opts)
		s.mu.Lock()
		s.nextStream = reverseStreamBase - 2
		s.mu.Unlock()
		if err := reverseReservePipe(t, s, "localhost:80"); err != nil {
			t.Fatal(err)
		}
		if err := reverseReservePipe(t, s, "localhost:80"); err == nil {
			t.Fatal("stream ID wrapped")
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.pending) != 1 || s.pending[0].stream != ^uint64(0) || s.nextStream != reverseStreamBase-1 {
			t.Fatal("invalid terminal stream ID")
		}
		if _, err := decodeFrame(s.pending[0].encode()); err != nil {
			t.Fatal("terminal stream ID cannot be decoded", err)
		}
	})
	t.Run("concurrent OPEN order", func(t *testing.T) {
		opts, _ := testOptions().defaults()
		opts.MaxStreams = 128
		s := reverseParkedSession(t, opts)
		var wg sync.WaitGroup
		errors := make(chan error, 128)
		// Register cleanup on the test goroutine, then race only admission itself.
		for i := 0; i < 128; i++ {
			conn, peer := net.Pipe()
			t.Cleanup(func() { conn.Close(); peer.Close() })
			wg.Add(1)
			go func() { defer wg.Done(); errors <- s.reserveReverse(conn, "localhost:80") }()
		}
		wg.Wait()
		close(errors)
		for err := range errors {
			if err != nil {
				t.Fatal(err)
			}
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.pending) != 128 {
			t.Fatal("missing OPENs")
		}
		for i, f := range s.pending {
			if f.seq != uint64(i+1) || f.stream != reverseStreamBase+uint64(i+1) {
				t.Fatalf("out-of-order OPEN at %d: %+v", i, f)
			}
		}
	})
}

func TestReverseMappingCountAndPortBoundaries(t *testing.T) {
	makeAccounts := func(total int) []Account {
		var accounts []Account
		for i := 0; i < total; i++ {
			if i%maxReverseMappings == 0 {
				accounts = append(accounts, Account{Username: fmt.Sprintf("user-%d", i/maxReverseMappings), Password: testPassword})
			}
			account := &accounts[len(accounts)-1]
			account.ReverseMappings = append(account.ReverseMappings, ReverseMapping{Listen: fmt.Sprintf("127.0.0.1:%d", 10000+i), Target: "localhost:80"})
		}
		return accounts
	}
	for _, count := range []int{0, 1, 256, 4096, 4097} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			accounts := makeAccounts(count)
			err := validateReverseMappings(accounts)
			if (err != nil) != (count > maxReverseListeners) {
				t.Fatalf("count %d: %v", count, err)
			}
		})
	}
	oversized := makeAccounts(257)
	oversized[0].ReverseMappings = append(oversized[0].ReverseMappings, oversized[1].ReverseMappings...)
	if err := validateReverseMappings(oversized[:1]); err == nil {
		t.Fatal("257 mappings accepted for one account")
	}
	for _, port := range []string{"1", "65535"} {
		if err := validateReverseMappings([]Account{{ReverseMappings: []ReverseMapping{{Listen: "[::1]:" + port, Target: "127.0.0.1:" + port}}}}); err != nil {
			t.Fatal("valid port boundary rejected", err)
		}
	}
	for _, port := range []string{"0", "65536", "-1", "+1", "01", " 1", "1\n"} {
		if err := validateReverseMappings([]Account{{ReverseMappings: []ReverseMapping{{Listen: "127.0.0.1:" + port, Target: "localhost:80"}}}}); err == nil {
			t.Fatal("invalid port accepted", port)
		}
	}
}

func FuzzReverseOpenFrame(f *testing.F) {
	for _, command := range []frame{
		{op: opReverseOpen, seq: 1, stream: reverseStreamBase + 1, data: []byte("127.0.0.1:22")},
		{op: opReverseOpen, seq: 1, stream: ^uint64(0), data: []byte("[::1]:65535")},
		{op: opReverseOpen, seq: 1, stream: 1, data: []byte("localhost:80")},
		{op: opReverseOpen, seq: 1, stream: reverseStreamBase + 1, data: bytes.Repeat([]byte("x"), chunkSize+1)},
	} {
		f.Add(command.encode())
	}
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, p []byte) {
		decoded, err := decodeFrame(p)
		if err != nil {
			return
		}
		if !bytes.Equal(decoded.encode(), p) {
			t.Fatal("accepted frame failed roundtrip")
		}
		if decoded.op == opReverseOpen && (decoded.seq == 0 || decoded.stream <= reverseStreamBase || validateSelectableTarget(string(decoded.data)) != nil) {
			t.Fatal("invalid reverse command accepted")
		}
	})
}

func TestReverseClientStreamLimitRecovery(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprintf("TLS=%t", secure), func(t *testing.T) {
			noiseRecoveryResourceCheck(t)
			opts := testOptions()
			opts.MaxStreams = 4
			s, web, address := reverseHarness(t, secure, targetBackend(t, "L"), opts)
			clientOpts := opts
			clientOpts.MaxStreams = 1
			link := connectReverseOptions(t, s, web, "alice", secure, clientOpts)
			remote := reverseRemote(t, s, link)
			first := dialReverse(t, address)
			readMarker(t, first, "L")
			for i := 0; i < 8; i++ {
				assertSocketClosed(t, dialReverse(t, address))
			}
			if err := exchange(first, bytes.Repeat([]byte("unaffected-stream"), 1024)); err != nil {
				t.Fatal("limit rejection broke admitted stream", err)
			}
			if err := reverseCloseWrite(first); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "limit slot released", func() bool { return reverseDrained(remote) && reverseDrained(link.local) })
			replacement := dialReverse(t, address)
			readMarker(t, replacement, "L")
			if err := exchange(replacement, []byte("slot-recovered")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReverseExpiryClosesRetainedSocketsAndRejectsResume(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprintf("TLS=%t", secure), func(t *testing.T) {
			noiseRecoveryResourceCheck(t)
			opts := testOptions()
			s, web, address := reverseHarness(t, secure, targetBackend(t, "E"), opts, 200*time.Millisecond)
			link := connectReverseOptions(t, s, web, "alice", secure, opts)
			remote := reverseRemote(t, s, link)
			conns := make([]net.Conn, 8)
			for i := range conns {
				conns[i] = dialReverse(t, address)
				readMarker(t, conns[i], "E")
			}
			link.wsClose()
			select {
			case <-link.done:
			case <-time.After(3 * time.Second):
				t.Fatal("transport did not stop")
			}
			waitFor(t, "expired reverse session removed", func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.sessions[link.id] == nil })
			for _, conn := range conns {
				assertSocketClosed(t, conn)
			}
			assertReleased(t, "expired reverse session resources", remote)
			ws, _, status, err := link.client.dial(context.Background(), link.id, true)
			if ws != nil {
				ws.Close()
			}
			if err == nil || status != http.StatusGone {
				t.Fatalf("expired resume: status=%d err=%v", status, err)
			}
			link.local.close()
			assertReleased(t, "expired client resources", link.local)
			fresh := connectReverseOptions(t, s, web, "alice", secure, opts)
			conn := dialReverse(t, address)
			readMarker(t, conn, "E")
			conn.SetDeadline(time.Now().Add(time.Second))
			if err := exchange(conn, []byte("fresh-session")); err != nil {
				t.Fatal(err)
			}
			if reverseRemote(t, s, fresh) == remote {
				t.Fatal("expired session was reused")
			}
		})
	}
}

// A malformed command must not make a target connection or consume stream state.
func TestReverseRejectedCommandsDoNotDial(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	opts, _ := testOptions().defaults()
	opts.receiveReverse = true
	for _, target := range []string{"", "localhost:0", "localhost:65536", "http://localhost:80", "localhost:80\n", string([]byte{0xff})} {
		s := newSession(context.Background(), "", opts)
		if err := s.apply(frame{op: opReverseOpen, stream: reverseStreamBase + 1, data: []byte(target)}); err == nil {
			t.Errorf("malformed reverse target accepted: %q", target)
		}
		s.mu.Lock()
		if len(s.streams) != 0 || s.lastOpened != 0 {
			t.Error("invalid OPEN consumed stream state")
		}
		s.mu.Unlock()
		s.close()
	}
	// Also exercise wrong-direction forward commands using an actual reachable target.
	s := newSession(context.Background(), backend.Addr().String(), opts)
	defer s.close()
	if err := s.apply(frame{op: opOpen, stream: reverseStreamBase + 1}); err == nil {
		t.Fatal("reverse ID accepted as forward")
	}
	backend.(*net.TCPListener).SetDeadline(time.Now().Add(30 * time.Millisecond))
	if conn, err := backend.Accept(); err == nil {
		conn.Close()
		t.Fatal("rejected command dialed a target")
	} else if network, ok := err.(net.Error); !ok || !network.Timeout() {
		t.Fatal(err)
	}
}
