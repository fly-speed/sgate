package tunnel

import (
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func stressWaitFor(t *testing.T, what string, ready func() bool) {
	t.Helper()
	stressWaitForBudget(t, what, 20*time.Second, ready)
}

func stressWaitForBudget(t *testing.T, what string, budget time.Duration, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Lifecycle probes are intentionally interrupted, and are excluded from the
// completed duplex byte totals. Any delivered bytes must be an exact prefix;
// neither a timeout nor extra bytes is accepted as a successful close.
type stressLifecycleProbe struct {
	gate           chan struct{}
	gateOnce       sync.Once
	ready          atomic.Int64
	wg             sync.WaitGroup
	errors         chan error
	payloadBytes   int
	timeout        time.Duration
	halfClose      bool
	results        chan stressProbeResult
	backendClients sync.Map
	connections    sync.Map
}

type stressProbeResult struct {
	seed     uint64
	backend  bool
	complete bool
}

func (p *stressLifecycleProbe) start() { p.gateOnce.Do(func() { close(p.gate) }) }

func stressLifecyclePattern(seed uint64, size int) []byte {
	p := make([]byte, size)
	_, _ = newStressByteStream(seed).Read(p)
	return p
}

func (p *stressLifecycleProbe) exchange(c net.Conn, seed uint64, backend bool) {
	defer p.wg.Done()
	defer c.Close()
	p.connections.Store(c, backend)
	defer p.connections.Delete(c)
	timeout := p.timeout
	if timeout == 0 {
		timeout = 40 * time.Second
	}
	_ = c.SetDeadline(time.Now().Add(timeout))
	// Distinct per-stream and per-direction patterns also detect stale routing.
	pattern := func(seed uint64) []byte { return stressLifecyclePattern(seed, p.payloadBytes) }
	tx, rx := seed, seed^0xa0761d6478bd642f
	if backend {
		tx, rx = rx, tx
	}
	p.ready.Add(1)
	<-p.gate
	sent := make(chan struct{})
	var writeErr error
	go func() {
		defer close(sent)
		_, writeErr = io.Copy(c, bytes.NewReader(pattern(tx)))
		if writeErr == nil && p.halfClose {
			writeErr = c.(interface{ CloseWrite() error }).CloseWrite()
		}
	}()
	got, err := io.ReadAll(io.LimitReader(c, int64(p.payloadBytes+1)))
	// Check every delivered byte, but do not synthesize an unread tail after
	// expiry. Thousands of empty prefixes must not create another payload-sized
	// allocation/CPU burst exactly when cleanup deadlines are being observed.
	if len(got) > p.payloadBytes || !bytes.Equal(got, stressLifecyclePattern(rx, len(got))) {
		p.errors <- fmt.Errorf("lifecycle probe %d: duplicate, corrupt or misrouted prefix", seed)
	}
	if e, ok := err.(net.Error); ok && e.Timeout() {
		p.errors <- fmt.Errorf("lifecycle probe %d did not close", seed)
	}
	if !p.halfClose || err != nil || len(got) != p.payloadBytes {
		c.Close() // Unblock a sender when expiry truncates the receive stream.
	}
	<-sent
	if p.results != nil {
		p.results <- stressProbeResult{
			seed:     seed,
			backend:  backend,
			complete: len(got) == p.payloadBytes && err == nil && writeErr == nil,
		}
	}
}

func TestStressLifecycleVerifierRejectsInvalidPrefixes(t *testing.T) {
	const seed, size = uint64(42), 1024
	want := stressLifecyclePattern(seed^0xa0761d6478bd642f, size)
	cases := []struct {
		name  string
		data  []byte
		valid bool
	}{
		{"empty", nil, true},
		{"prefix", want[:512], true},
		{"missing-byte", want[1:512], false},
		{"repeated-eight-bytes", append(append([]byte(nil), want[:8]...), want[:512]...), false},
		{"extra-byte", append(append([]byte(nil), want...), 1), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			app, peer := net.Pipe()
			p := &stressLifecycleProbe{gate: make(chan struct{}), errors: make(chan error, 2), payloadBytes: size}
			p.wg.Add(1)
			p.start()
			drained := make(chan struct{})
			go func() {
				defer close(drained)
				_, _ = io.Copy(io.Discard, peer)
			}()
			go p.exchange(app, seed, false)
			_, _ = peer.Write(c.data)
			peer.Close()
			p.wg.Wait()
			<-drained
			select {
			case err := <-p.errors:
				if c.valid {
					t.Fatal(err)
				}
			default:
				if !c.valid {
					t.Fatal("invalid lifecycle prefix was accepted")
				}
			}
		})
	}
}

func (p *stressLifecycleProbe) backend(c net.Conn, seed uint64) {
	p.backendClients.Store(c.RemoteAddr().String(), int((seed>>32)&0xffff))
	p.exchange(c, seed, true)
}

type stressLifecycleContext struct {
	kind                         string
	round, streams, socketBuffer int
	addrs                        []string
	states                       []*clientSessions
	server                       **Server
	httpServer                   **httptest.Server
	serverConfig                 ServerConfig
	startHTTP                    func(*Server, string) *httptest.Server
	offline                      *atomic.Bool
	peer                         *atomic.Pointer[stressLifecycleProbe]
	backendCount, localCount     *connectionCounter
	applicationConns             *sync.Map
	attached                     func() bool
	sample                       func(string, int, bool) stressSample
	relay                        *weakRelay
	boundaryOutage               time.Duration
	report                       *stressReport
	probeNonces                  *sync.Map
	probeURL                     string
	probeTLS                     *tls.Config
}

func stressLifecyclePhase(t *testing.T, f stressLifecycleContext) {
	t.Helper()
	n := len(f.addrs) * f.streams
	p := &stressLifecycleProbe{gate: make(chan struct{}), errors: make(chan error, 4*n),
		payloadBytes: max(32*1024, f.serverConfig.Options.BufferBytes/f.streams+chunkSize)}
	defaultRetention := f.serverConfig.SessionTTL == 0
	if defaultRetention {
		p.timeout = 160 * time.Second
	}
	if f.kind == "tls-timeout" || f.kind == "abandoned" || f.kind == "silent" || f.kind == "control" ||
		f.kind == "fragmented" {
		retention := f.serverConfig.SessionTTL
		if retention == 0 {
			retention = 2 * time.Minute
		}
		p.timeout = retention + 40*time.Second
	}
	if f.boundaryOutage > 0 {
		p.halfClose, p.timeout = true, 180*time.Second
		p.results = make(chan stressProbeResult, 2*n)
	}
	p.wg.Add(2 * n)
	f.peer.Store(p)
	var apps []net.Conn
	defer func() {
		p.start()
		for _, c := range apps {
			c.Close()
			f.applicationConns.Delete(c)
		}
	}()
	for client, addr := range f.addrs {
		for stream := 0; stream < f.streams; stream++ {
			c, err := net.DialTimeout("tcp", addr, stressApplicationDialTimeout)
			if err != nil {
				t.Fatal(err)
			}
			apps = append(apps, c)
			f.applicationConns.Store(c, true)
			if f.socketBuffer > 0 {
				if err := stressSocketBuffer(c, f.socketBuffer); err != nil {
					t.Fatal(err)
				}
			}
			seed := uint64(f.round)<<48 | uint64(client)<<32 | uint64(stream)
			var hello [16]byte
			binary.BigEndian.PutUint64(hello[:8], seed)
			if _, err := c.Write(hello[:]); err != nil {
				t.Fatal(err)
			}
			go p.exchange(c, seed, false)
		}
	}
	stressWaitFor(t, "all lifecycle probe peers", func() bool { return p.ready.Load() == int64(2*n) })
	oldServer := *f.server
	f.report.BackendSocketsBound += stressBoundBackendSockets(t, oldServer, f.socketBuffer, n)
	var remotes, clients []*session
	oldServer.mu.Lock()
	for _, e := range oldServer.sessions {
		remotes = append(remotes, e.session)
	}
	oldServer.mu.Unlock()
	for _, state := range f.states {
		state.mu.Lock()
		clients = append(clients, state.current)
		state.mu.Unlock()
	}
	if f.boundaryOutage > 0 {
		remotes = stressBoundaryOrderRemotes(t, p, remotes, len(clients))
	}
	f.offline.Store(true)
	outageStart := time.Now()
	activeRestart := f.kind == "restart" && f.relay != nil
	if activeRestart {
		// Stop a live server with blocked writers, running timers and full replay queues.
		for i := range f.relay.blackhole {
			f.relay.blackhole[i].Store(true)
		}
		defer func() {
			for i := range f.relay.blackhole {
				f.relay.blackhole[i].Store(false)
			}
		}()
	} else {
		for _, s := range remotes {
			s.mu.Lock()
			if s.active != nil {
				s.active.Close()
			}
			s.mu.Unlock()
		}
		for _, s := range append(append([]*session(nil), remotes...), clients...) {
			assertRecoveryNoiseStopped(t, s)
		}
	}
	p.start()
	stressWaitFor(t, "full lifecycle replay queues on both peers", func() bool {
		for _, s := range append(append([]*session(nil), remotes...), clients...) {
			s.mu.Lock()
			full := s.pendingBytes > 128*1024
			s.mu.Unlock()
			if !full {
				return false
			}
		}
		return true
	})
	f.sample(f.kind+"-buffered", f.round, true)
	if f.kind == "tls-timeout" || f.kind == "abandoned" || f.kind == "silent" || f.kind == "control" ||
		f.kind == "fragmented" {
		stressUnconfirmedRecovery(t, f, p, outageStart, clients, remotes)
		return
	}
	if f.boundaryOutage > 0 {
		stressBoundaryRecovery(t, f, p, outageStart, clients, remotes)
		return
	}
	if f.kind == "restart" {
		addr := (*f.httpServer).Listener.Addr().String()
		oldServer.Close()
		(*f.httpServer).Close()
		if c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond); err == nil {
			c.Close()
			t.Fatal("old HTTPS listener survived restart")
		}
		oldServer.mu.Lock()
		clean := len(oldServer.sessions) == 0 && len(oldServer.nonces) == 0
		oldServer.mu.Unlock()
		if !clean {
			t.Fatal("old server caches survived restart")
		}
		s, err := NewServer(f.serverConfig)
		if err != nil {
			t.Fatal(err)
		}
		*f.server = s
		*f.httpServer = f.startHTTP(s, addr)
	} else {
		budget := 20 * time.Second
		if defaultRetention {
			budget = 125 * time.Second
		}
		allOld := append(append([]*session(nil), remotes...), clients...)
		frozen := make([]uint64, len(allOld))
		for i, s := range allOld {
			frozen[i] = recoveryNoiseSent(s)
		}
		logAt := time.Now().Add(30 * time.Second)
		stressWaitForBudget(t, "server sessions expired while offline", budget, func() bool {
			if defaultRetention {
				for i, s := range allOld {
					if recoveryNoiseSent(s) != frozen[i] {
						t.Fatal("padding continued during default retention outage")
					}
					if time.Since(outageStart) < 119*time.Second && sessionReleased(s) {
						t.Fatal("default retention expired early")
					}
				}
				if time.Now().After(logAt) {
					t.Logf(
						"default retention offline %s: old padding writers stopped; waiting for expiry",
						time.Since(outageStart).Round(time.Second),
					)
					logAt = time.Now().Add(30 * time.Second)
				}
			}
			for _, s := range remotes {
				if !sessionReleased(s) {
					return false
				}
			}
			oldServer.mu.Lock()
			empty := len(oldServer.sessions) == 0
			oldServer.mu.Unlock()
			return empty
		})
		if defaultRetention {
			stressWaitForBudget(t, "default client offline retention released", max(
				125*time.Second-time.Since(outageStart),
				time.Second,
			), func() bool {
				for _, s := range clients {
					if !sessionReleased(s) {
						return false
					}
				}
				return true
			})
			stressWaitFor(
				t,
				"old application sockets released before network recovery",
				func() bool { return f.backendCount.active.Load() == 0 && f.localCount.active.Load() == 0 },
			)
			f.sample("expiry-offline-expired", f.round, true)
			t.Logf(
				("default retention released all old sessions and sockets after %s, while network was " +
					"still blocked"),
				time.Since(outageStart).Round(time.Millisecond),
			)
		} else {
			for _, s := range clients {
				s.mu.Lock()
				preserved := !s.closed && s.pendingBytes > 0
				s.mu.Unlock()
				if !preserved {
					t.Fatal("client discarded state before authenticated expiry")
				}
			}
		}
	}
	for _, s := range remotes {
		assertReleased(t, "old lifecycle server session and timers", s)
	}
	if activeRestart {
		for i := range f.relay.blackhole {
			f.relay.blackhole[i].Store(false)
		}
	}
	f.offline.Store(false)
	stressWaitFor(t, "old client sessions rejected after restart or expiry", func() bool {
		for _, s := range clients {
			if !sessionReleased(s) {
				return false
			}
		}
		return true
	})
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("lifecycle probes did not exit")
	}
	close(p.errors)
	for err := range p.errors {
		t.Error(err)
	}
	for _, c := range apps {
		c.Close()
		f.applicationConns.Delete(c)
	}
	stressWaitFor(t, "fresh lifecycle transports", f.attached)
	stressWaitFor(
		t,
		"lifecycle application connections released",
		func() bool { return f.backendCount.active.Load() == 0 && f.localCount.active.Load() == 0 },
	)
	f.peer.Store(nil)
	f.sample(f.kind+"-recovered", f.round, true)
	t.Logf(
		("%s phase: %d interrupted streams released; old sessions, replay queues and transport " +
			"timers released; fresh transports connected"),
		f.kind,
		n,
	)
}
