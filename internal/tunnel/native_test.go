package tunnel

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
)

func nativeFixture(t *testing.T, handle func(net.Conn)) *fixture {
	return newFixtureWithTransport(t, time.Minute, handle, testOptions(), testOptions(), true, true)
}
func TestNativeQUICDuplexResumeHalfClose(t *testing.T) {
	f := nativeFixture(t, nil)
	conns := make([]net.Conn, 8)
	for i := range conns {
		conns[i] = f.dial(t)
		if err := exchange(conns[i], []byte("before")); err != nil {
			t.Fatal(err)
		}
	}
	f.disconnect()
	waitFor(t, "native detached", func() bool { return !f.isAttached() })
	var wg sync.WaitGroup
	for i, c := range conns {
		wg.Add(1)
		go func(c net.Conn, i int) {
			defer wg.Done()
			if err := exchange(c, bytes.Repeat([]byte{byte(i)}, 256<<10)); err != nil {
				t.Error(err)
			}
		}(c, i)
	}
	f.blocked.Store(false)
	wg.Wait()
	if f.accepted.Load() != 8 {
		t.Fatal("native resume reopened backend sockets", f.accepted.Load())
	}
	for _, c := range conns {
		if err := c.(*net.TCPConn).CloseWrite(); err != nil {
			t.Fatal(err)
		}
		var b [1]byte
		if n, err := c.Read(b[:]); n != 0 || err != io.EOF {
			t.Fatalf("half close n=%d err=%v", n, err)
		}
	}
	waitFor(t, "native metadata retired", func() bool {
		f.server.mu.Lock()
		defer f.server.mu.Unlock()
		for _, entry := range f.server.sessions {
			entry.session.mu.Lock()
			n := len(entry.session.native.flows)
			entry.session.mu.Unlock()
			if n != 0 {
				return false
			}
		}
		return true
	})
}
func TestNativeQUICSlowStreamIsolationAndResume(t *testing.T) {
	gate := make(chan struct{})
	var release sync.Once
	defer release.Do(func() { close(gate) })
	stats := NewStats()
	opts := testOptions()
	opts.Stats = stats
	f := newFixtureWithTransport(t, time.Minute, func(c net.Conn) {
		var marker [1]byte
		if _, err := io.ReadFull(c, marker[:]); err != nil {
			return
		}
		if marker[0] == 0 {
			<-gate
		}
		c.Write(marker[:])
		io.Copy(c, c)
	}, opts, opts, true, true)
	slow := f.dial(t)
	slow.SetDeadline(time.Now().Add(20 * time.Second))
	if _, err := slow.Write([]byte{0}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "slow backend", func() bool { return f.accepted.Load() == 1 })
	f.server.mu.Lock()
	for _, entry := range f.server.sessions {
		entry.session.mu.Lock()
		for _, st := range entry.session.streams {
			if tcp, ok := st.conn.(*net.TCPConn); ok {
				tcp.SetWriteBuffer(4096)
			}
		}
		entry.session.mu.Unlock()
	}
	f.server.mu.Unlock()
	data := make([]byte, 8<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		sent := make(chan error, 1)
		go func() { _, err := slow.Write(data); sent <- err }()
		out := make([]byte, len(data)+1)
		_, err := io.ReadFull(slow, out)
		if err == nil && !bytes.Equal(out[1:], data) {
			err = io.ErrUnexpectedEOF
		}
		if err == nil {
			err = <-sent
		}
		done <- err
	}()
	waitFor(t, "slow receive backpressure", func() bool {
		f.server.mu.Lock()
		defer f.server.mu.Unlock()
		for _, entry := range f.server.sessions {
			entry.session.mu.Lock()
			n := entry.session.incomingBytes
			entry.session.mu.Unlock()
			if n > 0 {
				return true
			}
		}
		return false
	})
	healthy := f.dial(t)
	healthy.SetDeadline(time.Now().Add(3 * time.Second))
	if err := exchange(healthy, []byte("healthy during slow flow")); err != nil {
		t.Fatal(err)
	}
	f.disconnect()
	waitFor(t, "native detach", func() bool { return !f.isAttached() })
	f.blocked.Store(false)
	healthy.SetDeadline(time.Now().Add(3 * time.Second))
	if err := exchange(healthy, []byte("healthy after resume")); err != nil {
		t.Fatal(err)
	}
	release.Do(func() { close(gate) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("slow resumed stream blocked")
	}
	if f.accepted.Load() != 2 {
		t.Fatal("backend connection replaced on resume", f.accepted.Load())
	}
	if stats.Snapshot().QUIC.ReplayBytes == 0 {
		t.Fatal("test did not exercise application replay")
	}
}
func nativeEndpoint(t *testing.T, s *Server) (string, *tls.Config) {
	t.Helper()
	web := httptest.NewTLSServer(nil)
	defer web.Close()
	roots := x509.NewCertPool()
	roots.AddCert(web.Certificate())
	listener, err := ListenQUIC("127.0.0.1:0", web.TLS, s.cfg.Options, s)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := &http.Server{Handler: s}
	go httpServer.Serve(listener)
	t.Cleanup(func() { httpServer.Close() })
	return "quic://" + listener.Addr().String() + "/tunnel", &tls.Config{RootCAs: roots}
}
func TestNativeQUICReverseResumeAndClose(t *testing.T) {
	backend := targetBackend(t, "R")
	reverse := reverseAddress(t)
	server, err := NewServer(ServerConfig{Key: bytes.Repeat([]byte{8}, 32), Target: backend, Options: testOptions(), Accounts: []Account{{Username: "alice", Password: testPassword, ReverseMappings: []ReverseMapping{{Listen: reverse, Target: backend}}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if err := server.StartReverse(); err != nil {
		t.Fatal(err)
	}
	endpoint, trust := nativeEndpoint(t, server)
	client, err := NewClient(ClientConfig{URL: endpoint, TLSConfig: trust, Key: server.cfg.Key, Username: "alice", Password: testPassword, AllowReverse: true, Options: testOptions()})
	if err != nil {
		t.Fatal(err)
	}
	opts := client.cfg.Options
	opts.nativeQUIC = true
	local := newSession(context.Background(), "", opts)
	defer local.close()
	id := randomID()
	var nt *nativeTransport
	var done chan error
	attach := func(resume bool) {
		var err error
		nt, _, _, err = client.dialNative(context.Background(), id, resume)
		if err != nil {
			t.Fatal(err)
		}
		done = make(chan error, 1)
		go func() { done <- local.runNative(nt, time.Time{}, nil) }()
		waitFor(t, "native reverse ready", func() bool {
			server.mu.Lock()
			defer server.mu.Unlock()
			return server.sessions[id] != nil && server.sessions[id].ready
		})
	}
	attach(false)
	defer func() { local.close(); nt.stop(); <-done }()
	conn := dialReverse(t, reverse)
	readMarker(t, conn, "R")
	if err := exchange(conn, bytes.Repeat([]byte("reverse"), 32768)); err != nil {
		t.Fatal(err)
	}
	nt.conn.CloseWithError(0, "test disconnect")
	<-done
	waitFor(t, "native reverse detach", func() bool { server.mu.Lock(); defer server.mu.Unlock(); return !server.sessions[id].inUse })
	attach(true)
	if err := exchange(conn, []byte("reverse resumed")); err != nil {
		t.Fatal(err)
	}
	local.close()
	waitFor(t, "native authenticated close", func() bool { server.mu.Lock(); defer server.mu.Unlock(); return server.sessions[id] == nil })
}
func TestNativeQUICMetricsAndFlowChurn(t *testing.T) {
	opts := testOptions()
	stats := NewStats()
	opts.Stats = stats
	f := newFixtureWithTransport(t, time.Minute, nil, opts, opts, true, true)
	for i := 0; i < 64; i++ {
		c := f.dial(t)
		if err := exchange(c, []byte("churn")); err != nil {
			t.Fatal(err)
		}
		c.(*net.TCPConn).CloseWrite()
		io.Copy(io.Discard, c)
		c.Close()
	}
	waitFor(t, "flow metadata bounded", func() bool {
		f.server.mu.Lock()
		defer f.server.mu.Unlock()
		for _, e := range f.server.sessions {
			e.session.mu.Lock()
			n := len(e.session.native.flows)
			e.session.mu.Unlock()
			if n > 1 {
				return false
			}
		}
		return true
	})
	snapshot := stats.Snapshot()
	if snapshot.QUIC.PacketBytesTX == 0 || snapshot.QUIC.PacketBytesRX == 0 || snapshot.QUIC.NativeStreams < 64 || snapshot.QUIC.Active == 0 {
		t.Fatalf("missing QUIC metrics: %+v", snapshot.QUIC)
	}
}

func TestNativeQUICAuthenticationAndSessionBinding(t *testing.T) {
	server, err := NewServer(ServerConfig{Key: bytes.Repeat([]byte{8}, 32), Password: testPassword, Target: targetBackend(t, ""), Options: testOptions()})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	endpoint, trust := nativeEndpoint(t, server)
	cfg := ClientConfig{Key: server.cfg.Key, Password: testPassword, URL: endpoint, TLSConfig: trust, Options: testOptions()}
	for _, failure := range []string{"key", "password", "hostname"} {
		t.Run(failure, func(t *testing.T) {
			bad := cfg
			switch failure {
			case "key":
				bad.Key = bytes.Repeat([]byte{9}, 32)
			case "password":
				bad.Password = "wrong-password"
			case "hostname":
				bad.TLSConfig = trust.Clone()
				bad.TLSConfig.ServerName = "wrong.invalid"
			}
			client, err := NewClient(bad)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			nt, _, _, err := client.dialNative(ctx, randomID(), false)
			if nt != nil {
				nt.stop()
			}
			if err == nil {
				t.Fatal("bad native identity accepted")
			}
		})
	}
	client, _ := NewClient(cfg)
	if nt, _, status, err := client.dialNative(context.Background(), randomID(), true); err == nil || nt != nil || status != http.StatusGone {
		t.Fatalf("missing native session status=%d err=%v", status, err)
	}
	id := randomID()
	nt, _, _, err := client.dialNative(context.Background(), id, false)
	if err != nil {
		t.Fatal(err)
	}
	opts := client.cfg.Options
	opts.nativeQUIC = true
	local := newSession(context.Background(), "", opts)
	done := make(chan error, 1)
	go func() { done <- local.runNative(nt, time.Time{}, nil) }()
	defer local.close()
	waitFor(t, "native confirmed", func() bool {
		server.mu.Lock()
		defer server.mu.Unlock()
		return server.sessions[id] != nil && server.sessions[id].ready
	})
	nt.conn.CloseWithError(nativeSessionCloseCode, "forged proof")
	<-done
	waitFor(t, "native detached", func() bool { server.mu.Lock(); defer server.mu.Unlock(); return !server.sessions[id].inUse })
	// A legacy transport cannot reinterpret retained per-flow sequences as the
	// old global sequence space, even with the correct credentials.
	ws, _, status, err := client.dial(context.Background(), id, true)
	if ws != nil {
		ws.Close()
	}
	if err == nil || status != http.StatusConflict {
		t.Fatalf("protocol switch accepted status=%d err=%v", status, err)
	}
}

func TestNativeQUICUDPImpairment(t *testing.T) {
	stats := NewStats()
	opts := testOptions()
	opts.Stats = stats
	opts.HeartbeatTimeout = 5 * time.Second
	opts.BufferBytes = 1 << 20
	server, err := NewServer(ServerConfig{Key: bytes.Repeat([]byte{8}, 32), Password: testPassword, Target: targetBackend(t, ""), Options: opts})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	endpoint, trust := nativeEndpoint(t, server)
	u, _ := url.Parse(endpoint)
	relay := newUDPImpairment(t, u.Host, 5*time.Millisecond, 31)
	u.Host = relay.listener.LocalAddr().String()
	client, err := NewClient(ClientConfig{Key: server.cfg.Key, Password: testPassword, URL: u.String(), TLSConfig: trust, Options: opts})
	if err != nil {
		t.Fatal(err)
	}
	local, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx, local) }()
	defer func() { cancel(); <-done }()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		conn, err := net.Dial("tcp", local.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		conn.SetDeadline(time.Now().Add(15 * time.Second))
		defer conn.Close()
		wg.Add(1)
		go func(conn net.Conn, i int) {
			defer wg.Done()
			if err := exchange(conn, bytes.Repeat([]byte{byte(i)}, 128<<10)); err != nil {
				t.Error(err)
			}
		}(conn, i)
	}
	wg.Wait()
	if relay.drops.Load() == 0 || stats.Snapshot().QUIC.LostPackets == 0 {
		t.Fatal("test did not exercise UDP loss")
	}
}

func TestNativeQUICNoiseDoesNotBlockControl(t *testing.T) {
	stats := NewStats()
	opts := testOptions()
	opts.Stats = stats
	opts.Heartbeat = 20 * time.Millisecond
	opts.DialTimeout = 10 * time.Millisecond
	opts.WriteTimeout = 100 * time.Millisecond
	opts.HeartbeatTimeout = 2 * time.Second
	opts.NoiseInterval = time.Millisecond
	opts.NoiseMinBytes = chunkSize
	opts.NoiseMaxBytes = chunkSize
	f := newFixtureWithTransport(t, time.Minute, nil, opts, opts, true, true)
	conn := f.dial(t)
	for i := 0; i < 50; i++ {
		if err := exchange(conn, []byte("control is independent")); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	snapshot := stats.Snapshot()
	if snapshot.NoiseTX == 0 || snapshot.NoiseRX == 0 || snapshot.NoiseActivity.Disconnects != 0 {
		t.Fatalf("noise/control stalled: %+v", snapshot)
	}
}

func TestNativeQUICIntentionalCloseDuringNoise(t *testing.T) {
	stats := NewStats()
	opts := testOptions()
	opts.Stats = stats
	opts.NoiseInterval = time.Millisecond
	opts.NoiseMinBytes = chunkSize
	opts.NoiseMaxBytes = chunkSize
	server, err := NewServer(ServerConfig{Key: bytes.Repeat([]byte{8}, 32), Target: targetBackend(t, ""), Options: opts, Accounts: []Account{{Username: "alice", Password: testPassword}}})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	endpoint, trust := nativeEndpoint(t, server)
	client, err := NewClient(ClientConfig{Key: server.cfg.Key, Username: "alice", Password: testPassword, URL: endpoint, TLSConfig: trust, Options: opts})
	if err != nil {
		t.Fatal(err)
	}
	id := randomID()
	nt, _, _, err := client.dialNative(context.Background(), id, false)
	if err != nil {
		t.Fatal(err)
	}
	localOpts := client.cfg.Options
	localOpts.nativeQUIC = true
	local := newSession(context.Background(), "", localOpts)
	defer local.close()
	done := make(chan error, 1)
	go func() { done <- local.runNative(nt, time.Time{}, nil) }()
	waitFor(t, "native noise active", func() bool { return stats.Snapshot().NoiseTX >= 2*chunkSize })
	local.close()
	<-done
	waitFor(t, "named session removed during noise", func() bool { server.mu.Lock(); defer server.mu.Unlock(); return server.sessions[id] == nil })
}

func TestNativeQUICOutgoingOnlyTrafficSuppressesNoise(t *testing.T) {
	serverOpts := testOptions()
	serverOpts.NoiseSuppressTraffic = true
	serverOpts.NoiseInterval = time.Millisecond
	f := newFixtureWithTransport(t, time.Minute, func(c net.Conn) { c.Write([]byte("server push")); var b [1]byte; c.Read(b[:]) }, testOptions(), serverOpts, true, true)
	conn := f.dial(t)
	readMarker(t, conn, "server push")
	f.server.mu.Lock()
	defer f.server.mu.Unlock()
	for _, entry := range f.server.sessions {
		if entry.session.noiseAllowed(entry.session.opts, time.Now()) {
			t.Fatal("outgoing-only business traffic classified as idle")
		}
	}
}
