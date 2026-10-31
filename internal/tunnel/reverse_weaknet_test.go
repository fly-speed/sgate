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
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type reverseWeakFixture struct {
	server               *Server
	web                  *httptest.Server
	relay                *weakRelay
	mapping              ReverseMapping
	client               *Client
	state                clientSessions
	remote               *session
	backendCount         connectionCounter
	backendBytes         atomic.Int64
	serverLog, clientLog failureLogs
	trust                *x509.CertPool
	secure               bool
	stop                 func()
}

type reverseHalfCloseFaultListener struct {
	net.Listener
	accepted atomic.Int64
}

func (l *reverseHalfCloseFaultListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil && l.accepted.Add(1) == 1 {
		return reverseHalfCloseNotConnected{c}, nil
	}
	return c, err
}

type reverseHalfCloseNotConnected struct{ net.Conn }

func (c reverseHalfCloseNotConnected) CloseWrite() error {
	return &net.OpError{Op: "close", Net: "tcp", Source: c.LocalAddr(), Addr: c.RemoteAddr(), Err: &os.SyscallError{Syscall: "shutdown", Err: syscall.ENOTCONN}}
}

func newReverseWeakFixture(t *testing.T, wss, endpointTLS, fault bool, heartbeatTimeout time.Duration) *reverseWeakFixture {
	t.Helper()
	noiseRecoveryResourceCheck(t)
	f := &reverseWeakFixture{secure: endpointTLS, trust: x509.NewCertPool()}
	opts := Options{BufferBytes: 128 * 1024, MaxStreams: 128, Heartbeat: 100 * time.Millisecond, HeartbeatTimeout: heartbeatTimeout, DialTimeout: 400 * time.Millisecond, WriteTimeout: 400 * time.Millisecond, NoiseIntervalMin: 15 * time.Millisecond, NoiseIntervalMax: 35 * time.Millisecond, NoiseMinBytes: 128, NoiseMaxBytes: 512, Logf: f.serverLog.logf}
	var pair tls.Certificate
	if endpointTLS {
		var cert *x509.Certificate
		pair, cert, _, _ = reverseTestCertificate(t, 101)
		f.trust.AddCert(cert)
	}
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var listener net.Listener = countedListener{backend, &f.backendCount}
	if endpointTLS {
		listener = tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12})
	}
	var conns sync.Map
	var workers sync.WaitGroup
	backendDone := make(chan struct{})
	go func() {
		defer close(backendDone)
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			conns.Store(c, true)
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer conns.Delete(c)
				defer c.Close()
				if _, err := c.Write([]byte("R")); err == nil {
					io.Copy(c, readCounter{c, &f.backendBytes})
				}
			}()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		<-backendDone
		conns.Range(func(k, v any) bool { k.(net.Conn).Close(); return true })
		workers.Wait()
	})
	f.mapping = ReverseMapping{Listen: reverseAddress(t), Target: backend.Addr().String(), ListenTLS: endpointTLS, TargetTLS: endpointTLS}
	cfg := ServerConfig{Key: bytes.Repeat([]byte{39}, 32), Target: "127.0.0.1:2900", Accounts: []Account{{Username: "test", Password: testPassword, ReverseMappings: []ReverseMapping{f.mapping}}}, Options: opts, SessionTTL: 8 * time.Second}
	if endpointTLS {
		cfg.ReverseTLSConfig = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	}
	if fault {
		cfg.ReverseListen = func(network, address string) (net.Listener, error) {
			l, err := net.Listen(network, address)
			if err != nil {
				return nil, err
			}
			return &reverseHalfCloseFaultListener{Listener: l}, nil
		}
	}
	f.server, err = NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.server.Close)
	if err = f.server.StartReverse(); err != nil {
		t.Fatal(err)
	}
	f.web = httptest.NewUnstartedServer(f.server)
	if wss {
		f.web.StartTLS()
		f.trust.AddCert(f.web.Certificate())
	} else {
		f.web.Start()
	}
	t.Cleanup(func() { f.server.Close(); f.web.Close() })
	endpoint, _ := url.Parse(f.web.URL)
	f.relay = newWeakRelay(t, endpoint.Host)
	endpoint.Host = f.relay.listener.Addr().String()
	endpoint.Scheme = strings.Replace(endpoint.Scheme, "http", "ws", 1)
	endpoint.Path = "/tunnel"
	opts.Logf = f.clientLog.logf
	f.client, err = NewClient(ClientConfig{Key: cfg.Key, Username: "test", Password: testPassword, URL: endpoint.String(), AllowReverse: true, TLSConfig: &tls.Config{RootCAs: f.trust, MinVersion: tls.VersionTLS12}, Options: opts, ReconnectMin: 20 * time.Millisecond, ReconnectMax: 80 * time.Millisecond, OfflineTimeout: 8 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	local, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.client.run(ctx, local, &f.state) }()
	var once sync.Once
	f.stop = func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(4 * time.Second):
				t.Error("reverse weak client failed to stop")
			}
			f.server.Close()
			f.relay.close()
			if f.remote != nil {
				assertReleased(t, "weak reverse remote", f.remote)
			}
			if client := f.clientSession(); client != nil {
				assertReleased(t, "weak reverse local", client)
			}
		})
	}
	t.Cleanup(f.stop)
	waitFor(t, "authenticated reverse client ready", func() bool {
		f.server.mu.Lock()
		defer f.server.mu.Unlock()
		for _, entry := range f.server.sessions {
			if entry.ready {
				f.remote = entry.session
				return true
			}
		}
		return false
	})
	waitFor(t, "random padding in both directions", func() bool {
		local := f.clientSession()
		local.mu.Lock()
		a := local.noiseReceived > 0
		local.mu.Unlock()
		f.remote.mu.Lock()
		b := f.remote.noiseReceived > 0
		f.remote.mu.Unlock()
		return a && b
	})
	return f
}
func (f *reverseWeakFixture) clientSession() *session {
	f.state.mu.Lock()
	defer f.state.mu.Unlock()
	return f.state.current
}
func (f *reverseWeakFixture) dial(t *testing.T) net.Conn {
	t.Helper()
	var c net.Conn
	var err error
	if f.secure {
		c, err = tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", f.mapping.Listen, &tls.Config{RootCAs: f.trust, MinVersion: tls.VersionTLS12})
	} else {
		c, err = net.DialTimeout("tcp", f.mapping.Listen, time.Second)
	}
	if err != nil {
		t.Fatal(err)
	}
	deadline := 30 * time.Second
	if os.Getenv("SGATE_REVERSE_STRESS") == "1" {
		deadline = 2 * time.Minute
	}
	c.SetDeadline(time.Now().Add(deadline))
	t.Cleanup(func() { c.Close() })
	readMarker(t, c, "R")
	return c
}
func finishReverseWeak(c net.Conn) error {
	if err := c.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
		return err
	}
	var b [1]byte
	n, err := c.Read(b[:])
	if n != 0 || err != io.EOF {
		return fmt.Errorf("unexpected FIN tail: %d %v", n, err)
	}
	return nil
}
func assertReverseWeakBounds(t *testing.T, sessions ...*session) {
	t.Helper()
	for _, s := range sessions {
		s.mu.Lock()
		bad := s.closed || s.active == nil || s.pendingBytes > s.opts.BufferBytes || s.incomingBytes > s.opts.BufferBytes
		s.mu.Unlock()
		if bad {
			t.Fatal("reverse session failed or exceeded its bounded buffers")
		}
	}
}

func TestReverseWeakNetworkDelayJitterAndShortBlackholes(t *testing.T) {
	for _, wss := range []bool{false, true} {
		for _, endpointTLS := range []bool{false, true} {
			t.Run(fmt.Sprintf("WSS=%t/endpointsTLS=%t", wss, endpointTLS), func(t *testing.T) {
				f := newReverseWeakFixture(t, wss, endpointTLS, false, 3*time.Second)
				const streams = 4
				connections := make([]net.Conn, streams)
				for i := range connections {
					connections[i] = f.dial(t)
				}
				accepted := f.relay.accepted.Load()
				targetCount := f.backendCount.accepted.Load()
				f.relay.profile.Store(&weakProfile{delay: 150 * time.Millisecond, jitter: 100 * time.Millisecond, lossPercent: 15, retransmit: 200 * time.Millisecond})
				results := make(chan error, streams)
				size := reverseLoad(128*1024, 512*1024)
				for i, c := range connections {
					go func(i int, c net.Conn) {
						label := []byte(fmt.Sprintf("stream=%d/weak record;", i))
						data := bytes.Repeat(label, size/len(label)+1)[:size]
						results <- exchange(c, data)
					}(i, c)
				}
				for i := 0; i < streams; i++ {
					if err := <-results; err != nil {
						t.Fatal(err)
					}
				}
				if f.relay.accepted.Load() != accepted {
					t.Fatal("delay/jitter falsely reconnected the reverse tunnel")
				}
				if f.relay.losses.Load() == 0 {
					t.Fatal("no synthetic retransmission delay exercised")
				}
				f.relay.profile.Store(&weakProfile{delay: 20 * time.Millisecond, jitter: 10 * time.Millisecond, lossPercent: 10, retransmit: 50 * time.Millisecond})
				for direction := 0; direction < 2; direction++ {
					f.relay.blackhole[direction].Store(true)
					opposite := f.relay.downBytes.Load()
					if direction == 1 {
						opposite = f.relay.upBytes.Load()
					}
					result := make(chan error, 1)
					go func() { result <- exchange(connections[0], bytes.Repeat([]byte{byte(direction + 1)}, 128*1024)) }()
					time.Sleep(250 * time.Millisecond)
					progressed := f.relay.downBytes.Load() > opposite
					if direction == 1 {
						progressed = f.relay.upBytes.Load() > opposite
					}
					f.relay.blackhole[direction].Store(false)
					if err := <-result; err != nil {
						t.Fatal(err)
					}
					if !progressed {
						t.Fatal("opposite direction stopped consuming heartbeats/noise")
					}
					if f.relay.accepted.Load() != accepted {
						t.Fatal("short directional blackhole falsely reconnected")
					}
				}
				if count := f.backendCount.accepted.Load(); count != targetCount {
					t.Fatalf("backend streams replaced: %d != %d", count, targetCount)
				}
				expected := int64(streams*size + 2*128*1024)
				if got := f.backendBytes.Load(); got != expected {
					t.Fatalf("lost/duplicated upstream: %d != %d", got, expected)
				}
				assertReverseWeakBounds(t, f.remote, f.clientSession())
				f.relay.profile.Store(&weakProfile{})
				for _, c := range connections {
					if err := finishReverseWeak(c); err != nil {
						t.Fatal(err)
					}
				}
				waitFor(t, "weak streams and reliable buffers drained", func() bool { return reverseDrained(f.remote) && reverseDrained(f.clientSession()) })
				t.Logf("one-way=150±100ms retransmission=15%%/+200ms streams=%d verified_bytes_each_way=%d delayed_segments=%d", streams, expected, f.relay.losses.Load())
			})
		}
	}
}

func TestReverseWeakNetworkRepeatedDirectionalOutages(t *testing.T) {
	for _, wss := range []bool{false, true} {
		for _, endpointTLS := range []bool{false, true} {
			t.Run(fmt.Sprintf("WSS=%t/endpointsTLS=%t", wss, endpointTLS), func(t *testing.T) {
				f := newReverseWeakFixture(t, wss, endpointTLS, false, 900*time.Millisecond)
				connections := []net.Conn{f.dial(t), f.dial(t), f.dial(t), f.dial(t)}
				local, remote := f.clientSession(), f.remote
				targetCount := f.backendCount.accepted.Load()
				var expected int64
				for cycle := 0; cycle < reverseLoad(2, 6); cycle++ {
					f.relay.profile.Store(&weakProfile{delay: 20 * time.Millisecond, jitter: 10 * time.Millisecond, lossPercent: 10, retransmit: 50 * time.Millisecond})
					direction := cycle % 2
					accepted := f.relay.accepted.Load()
					f.relay.blackhole[direction].Store(true)
					results := make(chan error, len(connections))
					size := reverseLoad(128*1024, 512*1024)
					for i, c := range connections {
						go func(i int, c net.Conn) {
							label := []byte(fmt.Sprintf("cycle=%d stream=%d;", cycle, i))
							data := bytes.Repeat(label, size/len(label)+1)[:size]
							results <- exchange(c, data)
						}(i, c)
					}
					waitFor(t, "reverse heartbeat detected one-way outage and retried", func() bool { return f.relay.accepted.Load() > accepted })
					f.relay.blackhole[direction].Store(false)
					for range connections {
						if err := <-results; err != nil {
							t.Fatal(err)
						}
					}
					expected += int64(len(connections) * size)
					if f.clientSession() != local {
						t.Fatal("reconnect replaced client business session")
					}
					f.server.mu.Lock()
					retained := false
					for _, entry := range f.server.sessions {
						if entry.session == remote && entry.ready {
							retained = true
						}
					}
					f.server.mu.Unlock()
					if !retained || f.backendCount.accepted.Load() != targetCount {
						t.Fatal("reconnect replaced reverse target sockets")
					}
					assertReverseWeakBounds(t, local, remote)
					if got := f.backendBytes.Load(); got != expected {
						t.Fatalf("cycle %d repeated/lost data %d != %d", cycle, got, expected)
					}
				}
				f.relay.profile.Store(&weakProfile{})
				for _, c := range connections {
					if err := finishReverseWeak(c); err != nil {
						t.Fatal(err)
					}
				}
				waitFor(t, "replayed FIN and ACK drained", func() bool { return reverseDrained(local) && reverseDrained(remote) })
				t.Logf("outages=%d retained_streams=%d verified_bytes_each_way=%d", reverseLoad(2, 6), len(connections), expected)
			})
		}
	}
}

func TestReverseWeakNetworkHalfCloseENOTCONNIsolation(t *testing.T) {
	for _, wss := range []bool{false, true} {
		t.Run(fmt.Sprint("WSS=", wss), func(t *testing.T) {
			f := newReverseWeakFixture(t, wss, false, true, 3*time.Second)
			failed, healthy := f.dial(t), f.dial(t)
			accepted := f.relay.accepted.Load()
			f.relay.profile.Store(&weakProfile{delay: 80 * time.Millisecond, jitter: 40 * time.Millisecond, lossPercent: 15, retransmit: 100 * time.Millisecond})
			if err := exchange(failed, []byte("close this external client")); err != nil {
				t.Fatal(err)
			}
			if err := failed.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
				t.Fatal(err)
			}
			// The first accepted server-side socket deterministically reports the same
			// shutdown ENOTCONN as the field log when the client's reliable FIN arrives.
			assertSocketClosed(t, failed)
			waitFor(t, "ENOTCONN recorded on reverse stream", func() bool { return strings.Contains(f.serverLog.text(), "TCP half-close failed") })
			logs := f.serverLog.text()
			if !strings.Contains(logs, "flow=reverse") || !strings.Contains(logs, fmt.Sprintf("stream_target=%q", f.mapping.Target)) || !strings.Contains(logs, "local=") || !strings.Contains(logs, "shutdown:") {
				t.Fatal("misleading per-stream diagnostics", logs)
			}
			if strings.Count(logs, "TCP half-close failed") != 1 {
				t.Fatal("one failed flow produced duplicate half-close errors", logs)
			}
			if err := exchange(healthy, bytes.Repeat([]byte("healthy isolation record;"), 8192)); err != nil {
				t.Fatal(err)
			}
			fresh := f.dial(t)
			if err := exchange(fresh, []byte("new flow after ENOTCONN")); err != nil {
				t.Fatal(err)
			}
			if f.relay.accepted.Load() != accepted {
				t.Fatal("single ENOTCONN closed the entire tunnel")
			}
			assertReverseWeakBounds(t, f.remote, f.clientSession())
			f.relay.profile.Store(&weakProfile{})
			for _, c := range []net.Conn{healthy, fresh} {
				if err := finishReverseWeak(c); err != nil {
					t.Fatal(err)
				}
			}
			waitFor(t, "failed stream RESET and healthy FIN released", func() bool { return reverseDrained(f.remote) && reverseDrained(f.clientSession()) })
		})
	}
}

func TestReverseWeakNetworkExternalResetAndLateFIN(t *testing.T) {
	for _, wss := range []bool{false, true} {
		for _, endpointTLS := range []bool{false, true} {
			t.Run(fmt.Sprintf("WSS=%t/endpointsTLS=%t", wss, endpointTLS), func(t *testing.T) {
				f := newReverseWeakFixture(t, wss, endpointTLS, false, 3*time.Second)
				healthy := f.dial(t)
				accepted := f.relay.accepted.Load()
				f.relay.profile.Store(&weakProfile{delay: 40 * time.Millisecond, jitter: 20 * time.Millisecond, lossPercent: 10, retransmit: 80 * time.Millisecond})
				for i := 0; i < reverseLoad(12, 48); i++ {
					doomed := f.dial(t)
					if err := exchange(doomed, []byte(fmt.Sprintf("reset stream=%d", i))); err != nil {
						t.Fatal(err)
					}
					if err := doomed.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
						t.Fatal(err)
					}
					raw := doomed
					if secure, ok := raw.(*tls.Conn); ok {
						raw = secure.NetConn()
					}
					tcp := raw.(*net.TCPConn)
					if err := tcp.SetLinger(0); err != nil {
						t.Fatal("cannot inject external TCP RST", err)
					}
					if err := tcp.Close(); err != nil {
						t.Fatal(err)
					}
					if err := exchange(healthy, []byte(fmt.Sprintf("anchor still live=%d", i))); err != nil {
						t.Fatal(err)
					}
				}
				if f.relay.accepted.Load() != accepted {
					t.Fatal("external resets or late FIN replaced the tunnel")
				}
				fresh := f.dial(t)
				if err := exchange(fresh, []byte("fresh stream never receives stale bytes")); err != nil {
					t.Fatal(err)
				}
				f.relay.profile.Store(&weakProfile{})
				for _, c := range []net.Conn{healthy, fresh} {
					if err := finishReverseWeak(c); err != nil {
						t.Fatal(err)
					}
				}
				waitFor(t, "all reset and healthy streams released", func() bool { return reverseDrained(f.remote) && reverseDrained(f.clientSession()) })
			})
		}
	}
}

// Match the field report: two tunnels authenticate as the same account while
// their forward targets differ. Retained reverse flows stay on their original
// session; new reverse accepts may use the other confirmed online tunnel.
func TestReverseWeakNetworkSameAccountTwoTunnels(t *testing.T) {
	for _, wss := range []bool{false, true} {
		t.Run(fmt.Sprint("WSS=", wss), func(t *testing.T) {
			f := newReverseWeakFixture(t, wss, true, false, 900*time.Millisecond)
			original := f.dial(t)
			firstLocal := f.clientSession()
			firstRemote := f.remote
			if err := f.server.UpdateAllowedTargets([]string{"127.0.0.1:1982"}); err != nil {
				t.Fatal(err)
			}
			endpoint, _ := url.Parse(f.web.URL)
			otherRelay := newWeakRelay(t, endpoint.Host)
			endpoint.Host = otherRelay.listener.Addr().String()
			endpoint.Scheme = strings.Replace(endpoint.Scheme, "http", "ws", 1)
			endpoint.Path = "/tunnel"
			cfg := f.client.cfg
			cfg.URL = endpoint.String()
			cfg.Addr = "127.0.0.1:1982"
			other, err := NewClient(cfg)
			if err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			var state clientSessions
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- other.run(ctx, listener, &state) }()
			t.Cleanup(func() {
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(3 * time.Second):
					t.Error("second account tunnel retained")
				}
			})
			var secondRemote *session
			waitFor(t, "second same-account session ready", func() bool {
				f.server.mu.Lock()
				defer f.server.mu.Unlock()
				for _, entry := range f.server.sessions {
					if entry.session != firstRemote && entry.ready {
						secondRemote = entry.session
						return true
					}
				}
				return false
			})
			accepted := f.relay.accepted.Load()
			f.relay.blackhole[0].Store(true)
			baseline := []byte("retained reverse stream on first session")
			result := make(chan error, 1)
			go func() { result <- exchange(original, bytes.Repeat(baseline, 8192)) }()
			waitFor(t, "first session offline, other session remains ready", func() bool {
				f.server.mu.Lock()
				defer f.server.mu.Unlock()
				first, second := false, false
				for _, entry := range f.server.sessions {
					if entry.session == firstRemote {
						first = entry.ready
					}
					if entry.session == secondRemote {
						second = entry.ready
					}
				}
				return !first && second && f.relay.accepted.Load() > accepted
			})
			newFlow := f.dial(t)
			if err := exchange(newFlow, []byte("new reverse stream routed through second tunnel")); err != nil {
				t.Fatal(err)
			}
			f.relay.blackhole[0].Store(false)
			if err := <-result; err != nil {
				t.Fatal(err)
			}
			if f.clientSession() != firstLocal || f.backendCount.accepted.Load() != 2 {
				t.Fatal("account failover recreated retained target sockets")
			}
			secondRemote.mu.Lock()
			secondFlows := len(secondRemote.streams)
			secondRemote.mu.Unlock()
			if secondFlows != 1 {
				t.Fatal("new reverse flow was not isolated on the second tunnel")
			}
			for i, c := range []net.Conn{original, newFlow} {
				if err := exchange(c, bytes.Repeat([]byte(fmt.Sprintf("session=%d unique bytes;", i)), 2048)); err != nil {
					t.Fatal(err)
				}
				if err := finishReverseWeak(c); err != nil {
					t.Fatal(err)
				}
			}
			waitFor(t, "both same-account sessions drained", func() bool {
				state.mu.Lock()
				second := state.current
				state.mu.Unlock()
				return reverseDrained(firstLocal) && reverseDrained(firstRemote) && reverseDrained(second) && reverseDrained(secondRemote)
			})
			t.Log("same account, distinct forward targets, existing reverse stream resumed; new reverse flow used the other online session")
		})
	}
}
