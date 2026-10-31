package tunnel

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// Default workloads run in ordinary CI; SGATE_REVERSE_STRESS=1 increases
// concurrent sockets, bytes and reconnect cycles without changing assertions.
func reverseLoad(defaultValue, stressValue int) int {
	if os.Getenv("SGATE_REVERSE_STRESS") == "1" {
		return stressValue
	}
	return defaultValue
}

func reverseHarness(t *testing.T, secure bool, target string, opts Options, ttl ...time.Duration) (*Server, *httptest.Server, string) {
	t.Helper()
	address := reverseAddress(t)
	retention := 3 * time.Second
	if len(ttl) > 0 {
		retention = ttl[0]
	}
	s, err := NewServer(ServerConfig{Key: bytes.Repeat([]byte{8}, 32), Accounts: []Account{{Username: "alice", Password: testPassword, ReverseMappings: []ReverseMapping{{Listen: address, Target: target}}}}, Target: target, Options: opts, SessionTTL: retention})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if err = s.StartReverse(); err != nil {
		t.Fatal(err)
	}
	web := httptest.NewUnstartedServer(s)
	if secure {
		web.StartTLS()
	} else {
		web.Start()
	}
	t.Cleanup(func() { s.Close(); web.Close() })
	return s, web, address
}

func reverseRemote(t *testing.T, s *Server, link *reverseLink) *session {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.sessions[link.id]
	if entry == nil {
		t.Fatal("reverse session disappeared")
	}
	return entry.session
}

func reverseDrained(s *session) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed && len(s.streams) == 0 && s.pendingBytes == 0 && s.incomingBytes == 0
}

func reverseCloseWrite(conn net.Conn) error {
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		return err
	}
	rest, err := io.ReadAll(conn)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return fmt.Errorf("unexpected trailing bytes: %d", len(rest))
	}
	return nil
}

func TestReverseStressFanoutAndConnectionChurn(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprintf("TLS=%t", secure), func(t *testing.T) {
			noiseRecoveryResourceCheck(t)
			workers, waves, size := reverseLoad(64, 128), reverseLoad(4, 8), reverseLoad(128*1024, 256*1024)
			opts := testOptions()
			opts.MaxStreams = workers
			opts.BufferBytes = 256 * 1024
			s, web, address := reverseHarness(t, secure, targetBackend(t, "S"), opts)
			link := connectReverseOptions(t, s, web, "alice", secure, opts)
			remote := reverseRemote(t, s, link)
			started := time.Now()
			for wave := 0; wave < waves; wave++ {
				gate := make(chan struct{})
				results := make(chan error, workers)
				for worker := 0; worker < workers; worker++ {
					go func(worker int) {
						<-gate
						conn, err := net.DialTimeout("tcp", address, 3*time.Second)
						if err != nil {
							results <- err
							return
						}
						defer conn.Close()
						conn.SetDeadline(time.Now().Add(30 * time.Second))
						var marker [1]byte
						if _, err = io.ReadFull(conn, marker[:]); err != nil {
							results <- err
							return
						}
						if marker[0] != 'S' {
							results <- fmt.Errorf("wrong target marker %q", marker)
							return
						}
						payload := bytes.Repeat([]byte(fmt.Sprintf("wave=%04d worker=%04d record;", wave, worker)), size/28+1)[:size]
						if err = exchange(conn, payload); err == nil {
							err = reverseCloseWrite(conn)
						}
						results <- err
					}(worker)
				}
				close(gate)
				for worker := 0; worker < workers; worker++ {
					if err := <-results; err != nil {
						t.Errorf("wave %d: %v", wave, err)
					}
				}
				if t.Failed() {
					return
				}
				waitFor(t, "all churn streams and caches drained", func() bool { return reverseDrained(remote) && reverseDrained(link.local) })
			}
			t.Logf("connections=%d payload=%d MiB each direction elapsed=%s", workers*waves, workers*waves*size/(1024*1024), time.Since(started).Round(time.Millisecond))
		})
	}
}

func TestReverseStressReplayDuringRepeatedOutages(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprintf("TLS=%t", secure), func(t *testing.T) {
			noiseRecoveryResourceCheck(t)
			opts := testOptions()
			opts.BufferBytes = 64 * 1024
			s, web, address := reverseHarness(t, secure, targetBackend(t, "R"), opts)
			link := connectReverseOptions(t, s, web, "alice", secure, opts)
			remote := reverseRemote(t, s, link)
			streams, cycles := reverseLoad(8, 32), reverseLoad(4, 12)
			conns := make([]net.Conn, streams)
			for i := range conns {
				conns[i] = dialReverse(t, address)
				readMarker(t, conns[i], "R")
			}
			for cycle := 0; cycle < cycles; cycle++ {
				link.wsClose()
				select {
				case <-link.done:
				case <-time.After(3 * time.Second):
					t.Fatal("transport failed to detach")
				}
				waitFor(t, "server transport detached", func() bool { s.mu.Lock(); defer s.mu.Unlock(); return !s.sessions[link.id].inUse })
				assertSocketClosed(t, dialReverse(t, address))
				results := make(chan error, streams)
				for i, conn := range conns {
					conn.SetDeadline(time.Now().Add(20 * time.Second))
					go func(i int, c net.Conn) {
						payload := bytes.Repeat([]byte(fmt.Sprintf("cycle=%d stream=%d;", cycle, i)), 40000)
						results <- exchange(c, payload)
					}(i, conn)
				}
				waitFor(t, "offline reverse replay buffer under pressure", func() bool {
					remote.mu.Lock()
					defer remote.mu.Unlock()
					return remote.pendingBytes >= opts.BufferBytes-2*(chunkSize+frameHeader)
				})
				remote.mu.Lock()
				if remote.pendingBytes > opts.BufferBytes {
					t.Error("unbounded offline replay")
				}
				remote.mu.Unlock()
				link.attach(t, s, true)
				if reverseRemote(t, s, link) != remote {
					t.Fatal("reconnect replaced retained business session")
				}
				for i := 0; i < streams; i++ {
					if err := <-results; err != nil {
						t.Fatalf("cycle %d: %v", cycle, err)
					}
				}
			}
			for _, conn := range conns {
				if err := reverseCloseWrite(conn); err != nil {
					t.Fatal(err)
				}
			}
			waitFor(t, "resumed streams fully released", func() bool { return reverseDrained(remote) && reverseDrained(link.local) })
			t.Logf("retained streams=%d outages=%d", streams, cycles)
		})
	}
}

// The backend stops reading entirely. Shutdown must release both pending
// producers and the peer's blocked TCP writer without waiting for application IO.
func TestReverseSlowTargetBackpressureAndShutdown(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprintf("TLS=%t", secure), func(t *testing.T) {
			noiseRecoveryResourceCheck(t)
			gate := make(chan struct{})
			defer close(gate)
			backend, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			var connections sync.Map
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				conn, err := backend.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				connections.Store(conn, true)
				defer connections.Delete(conn)
				<-gate
				io.Copy(io.Discard, conn)
			}()
			t.Cleanup(func() {
				backend.Close()
				connections.Range(func(k, v any) bool { k.(net.Conn).Close(); return true })
				wg.Wait()
			})
			opts := testOptions()
			opts.BufferBytes = 64 * 1024
			opts.Heartbeat = 50 * time.Millisecond
			opts.HeartbeatTimeout = 3 * time.Second
			s, web, address := reverseHarness(t, secure, backend.Addr().String(), opts)
			link := connectReverseOptions(t, s, web, "alice", secure, opts)
			remote := reverseRemote(t, s, link)
			conn := dialReverse(t, address)
			result := make(chan error, 1)
			go func() { _, err := io.Copy(conn, io.LimitReader(&repeatByteReader{}, 64*1024*1024)); result <- err }()
			waitFor(t, "bounded slow target backpressure", func() bool {
				remote.mu.Lock()
				pending := remote.pendingBytes
				remote.mu.Unlock()
				link.local.mu.Lock()
				incoming := link.local.incomingBytes
				link.local.mu.Unlock()
				return pending >= opts.BufferBytes-2*(chunkSize+frameHeader) && incoming >= opts.BufferBytes-2*(chunkSize+frameHeader)
			})
			// More than one heartbeat passes while business delivery remains blocked.
			time.Sleep(200 * time.Millisecond)
			for _, session := range []*session{remote, link.local} {
				session.mu.Lock()
				bad := session.closed || session.active == nil || session.pendingBytes > opts.BufferBytes || session.incomingBytes > opts.BufferBytes
				session.mu.Unlock()
				if bad {
					t.Fatal("slow application broke transport or buffer limits")
				}
			}
			s.Close()
			link.local.close()
			conn.Close()
			select {
			case <-result:
			case <-time.After(3 * time.Second):
				t.Fatal("backpressured producer survived shutdown")
			}
			assertReleased(t, "reverse server shutdown", remote)
			assertReleased(t, "reverse client shutdown", link.local)
		})
	}
}

type repeatByteReader struct{}

func (*repeatByteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0x5a
	}
	return len(p), nil
}

// Account policy is repeatedly revoked and rebound while TCP admissions race it.
// The second account keeps its original stream and must never observe Alice's data.
func TestReverseStressAccountPolicyChurnIsolation(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprintf("TLS=%t", secure), func(t *testing.T) {
			noiseRecoveryResourceCheck(t)
			a, b, c := targetBackend(t, "A"), targetBackend(t, "B"), targetBackend(t, "C")
			aliceAddress, bobAddress := reverseAddress(t), reverseAddress(t)
			policy := func(target string, disabled bool) []Account {
				return []Account{
					{Username: "alice", Password: testPassword, Disabled: disabled, ReverseMappings: []ReverseMapping{{Listen: aliceAddress, Target: target}}},
					{Username: "bob", Password: testPassword, ReverseMappings: []ReverseMapping{{Listen: bobAddress, Target: b}}},
				}
			}
			opts := testOptions()
			s, err := NewServer(ServerConfig{Key: bytes.Repeat([]byte{8}, 32), Accounts: policy(a, false), Target: a, Options: opts})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(s.Close)
			if err = s.StartReverse(); err != nil {
				t.Fatal(err)
			}
			web := httptest.NewUnstartedServer(s)
			if secure {
				web.StartTLS()
			} else {
				web.Start()
			}
			t.Cleanup(func() { s.Close(); web.Close() })
			bob := connectReverseOptions(t, s, web, "bob", secure, opts)
			bobRemote := reverseRemote(t, s, bob)
			stable := dialReverse(t, bobAddress)
			readMarker(t, stable, "B")
			stable.SetDeadline(time.Now().Add(30 * time.Second))
			cfg := ClientConfig{AllowReverse: true, Key: s.cfg.Key, Username: "alice", Password: testPassword, URL: "ws" + strings.TrimPrefix(web.URL, "http") + "/tunnel", Options: opts, ReconnectMin: time.Millisecond, ReconnectMax: 5 * time.Millisecond}
			if secure {
				roots := x509.NewCertPool()
				roots.AddCert(web.Certificate())
				cfg.TLSConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
			}
			client, err := NewClient(cfg)
			if err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			clientDone := make(chan error, 1)
			go func() { clientDone <- client.Run(ctx, listener) }()
			t.Cleanup(func() {
				cancel()
				select {
				case err := <-clientDone:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(5 * time.Second):
					t.Error("auto-reconnecting client did not stop")
				}
			})
			online := func() bool {
				s.mu.Lock()
				defer s.mu.Unlock()
				for _, entry := range s.sessions {
					if entry.username == "alice" && entry.ready && entry.inUse {
						return true
					}
				}
				return false
			}
			waitFor(t, "Alice reverse client online", online)
			stop := make(chan struct{})
			results := make(chan error, 5)
			var wg sync.WaitGroup
			for i := 0; i < 4; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for {
						select {
						case <-stop:
							return
						default:
						}
						conn, err := net.DialTimeout("tcp", aliceAddress, 100*time.Millisecond)
						if err != nil {
							continue
						}
						conn.SetDeadline(time.Now().Add(time.Second))
						var marker [1]byte
						_, err = io.ReadFull(conn, marker[:])
						conn.Close()
						if err == nil && marker[0] != 'A' && marker[0] != 'C' {
							results <- fmt.Errorf("Alice received another account's marker %q", marker)
							return
						}
					}
				}()
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					if err := exchange(stable, bytes.Repeat([]byte("bob-isolated-record"), 256)); err != nil {
						results <- fmt.Errorf("unrelated account interrupted: %w", err)
						return
					}
				}
			}()
			// Always join load workers before fixture teardown, including Fatal paths.
			defer func() {
				close(stop)
				wg.Wait()
				close(results)
				for err := range results {
					t.Error(err)
				}
			}()
			for cycle := 0; cycle < reverseLoad(12, 48); cycle++ {
				if err = s.UpdateAccounts(policy(a, true)); err != nil {
					t.Fatal(err)
				}
				if online() {
					t.Fatal("disabled account retained an admitted session")
				}
				target, marker := a, "A"
				if cycle%2 == 1 {
					target, marker = c, "C"
				}
				if err = s.UpdateAccounts(policy(target, false)); err != nil {
					t.Fatal(err)
				}
				waitFor(t, "Alice reauthenticated after policy change", online)
				check := dialReverse(t, aliceAddress)
				readMarker(t, check, marker)
				check.Close()
				if reverseRemote(t, s, bob) != bobRemote {
					t.Fatal("policy churn replaced unrelated account session")
				}
			}
			t.Logf("policy disable/enable cycles=%d with four concurrent admission workers", reverseLoad(12, 48))
		})
	}
}
