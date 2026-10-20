package tunnel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type connectionCounter struct{ active, accepted atomic.Int64 }
type countedListener struct {
	net.Listener
	count *connectionCounter
}
type countedConn struct {
	net.Conn
	count *connectionCounter
	once  sync.Once
}

func (l countedListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.count.active.Add(1)
	l.count.accepted.Add(1)
	return &countedConn{Conn: c, count: l.count}, nil
}
func (c *countedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.count.active.Add(-1) })
	return err
}
func (c *countedConn) CloseWrite() error {
	return c.Conn.(interface{ CloseWrite() error }).CloseWrite()
}

type stressSample struct {
	Phase              string `json:"phase"`
	Round              int    `json:"round"`
	Milliseconds       int64  `json:"milliseconds"`
	HeapBytes          uint64 `json:"heap_bytes"`
	HeapInuseBytes     uint64 `json:"heap_inuse_bytes"`
	StackBytes         uint64 `json:"stack_bytes"`
	SysBytes           uint64 `json:"sys_bytes"`
	RSSBytes           int64  `json:"rss_bytes,omitempty"`
	Goroutines         int    `json:"goroutines"`
	FileDescriptors    int    `json:"file_descriptors"`
	BackendConnections int64  `json:"backend_connections"`
	LocalConnections   int64  `json:"local_connections"`
	HTTPConnections    int64  `json:"http_connections"`
	Sessions           int    `json:"sessions"`
	Streams            int    `json:"streams"`
	PendingBytes       int    `json:"pending_bytes"`
	Nonces             int    `json:"nonces"`
}
type stressReport struct {
	Clients                   int            `json:"clients"`
	StreamsPerClient          int            `json:"streams_per_client"`
	PayloadBytes              int            `json:"payload_bytes"`
	CompletedRounds           int            `json:"completed_rounds"`
	Disconnects               int64          `json:"disconnects"`
	VerifiedBytesPerDirection int64          `json:"verified_bytes_per_direction"`
	Samples                   []stressSample `json:"samples"`
}

func stressInt(t *testing.T, name string, fallback int) int {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	n, e := strconv.Atoi(v)
	if e != nil || n < 1 {
		t.Fatalf("invalid %s", name)
	}
	return n
}
func descriptorCount() int {
	for _, dir := range []string{"/proc/self/fd", "/dev/fd"} {
		if entries, err := os.ReadDir(dir); err == nil {
			return len(entries)
		}
	}
	// macOS does not always allow directory enumeration of /dev/fd.
	if out, err := exec.Command("lsof", "-a", "-p", strconv.Itoa(os.Getpid()), "-Ff").Output(); err == nil {
		n := 0
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "f") {
				if _, err := strconv.Atoi(strings.TrimPrefix(line, "f")); err == nil {
					n++
				}
			}
		}
		return n
	}
	return -1
}
func currentRSS() int64 {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	return n * 1024
}

// Opt in: SGATE_STRESS=1 go test -run '^TestStressReconnectResources$' -v ./internal/tunnel
func TestStressReconnectResources(t *testing.T) {
	if os.Getenv("SGATE_STRESS") != "1" {
		t.Skip("set SGATE_STRESS=1 to run the resource stress test")
	}
	initialGoroutines := runtime.NumGoroutine()
	initialDescriptors := descriptorCount()
	clients := stressInt(t, "SGATE_STRESS_CLIENTS", 4)
	streams := stressInt(t, "SGATE_STRESS_STREAMS", 64)
	rounds := stressInt(t, "SGATE_STRESS_ROUNDS", 12)
	payloadBytes := stressInt(t, "SGATE_STRESS_BYTES", 128*1024)
	minDuration := time.Duration(0)
	if v := os.Getenv("SGATE_STRESS_DURATION"); v != "" {
		var err error
		minDuration, err = time.ParseDuration(v)
		if err != nil {
			t.Fatal(err)
		}
	}
	report := stressReport{Clients: clients, StreamsPerClient: streams, PayloadBytes: payloadBytes}
	start := time.Now()
	var backendCount, localCount, httpCount connectionCounter
	listen := func(counter *connectionCounter) net.Listener {
		l, e := net.Listen("tcp", "127.0.0.1:0")
		if e != nil {
			t.Fatal(e)
		}
		return countedListener{l, counter}
	}
	backend := listen(&backendCount)
	var backendWG sync.WaitGroup
	var backendConns sync.Map
	var applicationConns sync.Map
	var reportDisconnects atomic.Int64
	var verifiedBytes atomic.Int64
	backendWG.Add(1)
	go func() {
		defer backendWG.Done()
		for {
			c, e := backend.Accept()
			if e != nil {
				return
			}
			backendConns.Store(c, true)
			backendWG.Add(1)
			go func() { defer backendWG.Done(); defer c.Close(); defer backendConns.Delete(c); _, _ = io.Copy(c, c) }()
		}
	}()
	opts := Options{BufferBytes: 256 * 1024, MaxStreams: streams + 16, Heartbeat: 5 * time.Second, DialTimeout: 2 * time.Second, WriteTimeout: 10 * time.Second, Logf: func(string, ...any) {}}
	key := bytes.Repeat([]byte{42}, 32)
	server, e := NewServer(ServerConfig{Key: key, Target: backend.Addr().String(), SessionTTL: 10 * time.Second, Options: opts})
	if e != nil {
		t.Fatal(e)
	}
	var offline atomic.Bool
	httpServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if offline.Load() {
			http.Error(w, "offline", 503)
			return
		}
		server.ServeHTTP(w, r)
	}))
	httpServer.Listener.Close()
	httpServer.Listener = listen(&httpCount)
	httpServer.Start()
	ctx, cancel := context.WithCancel(context.Background())
	var clientWG sync.WaitGroup
	addrs := make([]string, clients)
	for i := range addrs {
		l := listen(&localCount)
		addrs[i] = l.Addr().String()
		c, err := NewClient(ClientConfig{Key: key, URL: "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/tunnel", ReconnectMin: 5 * time.Millisecond, ReconnectMax: 25 * time.Millisecond, Options: opts})
		if err != nil {
			t.Fatal(err)
		}
		clientWG.Add(1)
		go func() {
			defer clientWG.Done()
			if err := c.Run(ctx, l); err != nil {
				t.Error(err)
			}
		}()
	}
	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			applicationConns.Range(func(k, v any) bool { k.(net.Conn).Close(); return true })
			cancel()
			clientWG.Wait()
			server.Close()
			httpServer.Close()
			backend.Close()
			backendConns.Range(func(k, v any) bool { k.(net.Conn).Close(); return true })
			backendWG.Wait()
		})
	}
	defer cleanup()
	attached := func() bool {
		server.mu.Lock()
		defer server.mu.Unlock()
		if len(server.sessions) != clients {
			return false
		}
		for _, e := range server.sessions {
			if !e.inUse {
				return false
			}
		}
		return true
	}
	waitFor(t, "all stress transports", attached)
	sample := func(phase string, round int, collect bool) stressSample {
		if collect {
			debug.FreeOSMemory()
		}
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		p := stressSample{Phase: phase, Round: round, Milliseconds: time.Since(start).Milliseconds(), HeapBytes: m.HeapAlloc, HeapInuseBytes: m.HeapInuse, StackBytes: m.StackInuse, SysBytes: m.Sys, Goroutines: runtime.NumGoroutine(), FileDescriptors: -1, BackendConnections: backendCount.active.Load(), LocalConnections: localCount.active.Load(), HTTPConnections: httpCount.active.Load()}
		if phase != "load" {
			p.FileDescriptors = descriptorCount()
		}
		if collect {
			p.RSSBytes = currentRSS()
		}
		server.mu.Lock()
		p.Sessions = len(server.sessions)
		p.Nonces = len(server.nonces)
		for _, e := range server.sessions {
			e.session.mu.Lock()
			p.Streams += len(e.session.streams)
			p.PendingBytes += e.session.pendingBytes
			e.session.mu.Unlock()
		}
		server.mu.Unlock()
		report.Samples = append(report.Samples, p)
		if collect {
			t.Logf("%s round=%d heap=%.2fMiB rss=%.2fMiB g=%d fd=%d backend=%d local=%d http=%d sessions=%d streams=%d pending=%d nonces=%d", phase, round, float64(p.HeapBytes)/(1<<20), float64(p.RSSBytes)/(1<<20), p.Goroutines, p.FileDescriptors, p.BackendConnections, p.LocalConnections, p.HTTPConnections, p.Sessions, p.Streams, p.PendingBytes, p.Nonces)
		}
		return p
	}
	base := sample("baseline", 0, true)
	if path := os.Getenv("SGATE_STRESS_REPORT"); path != "" {
		defer func() {
			b, err := json.MarshalIndent(report, "", "  ")
			if err == nil {
				err = os.MkdirAll(filepath.Dir(path), 0755)
			}
			if err == nil {
				err = os.WriteFile(path, append(b, '\n'), 0644)
			}
			if err != nil {
				t.Error(err)
			}
		}()
	}
	for round := 1; round <= rounds; round++ {
		roundEnd := time.Now().Add(minDuration / time.Duration(rounds))
		var conns []net.Conn
		for _, addr := range addrs {
			for i := 0; i < streams; i++ {
				c, err := net.DialTimeout("tcp", addr, 5*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				deadline := time.Now().Add(30 * time.Second)
				if roundEnd.Add(30 * time.Second).After(deadline) {
					deadline = roundEnd.Add(30 * time.Second)
				}
				_ = c.SetDeadline(deadline)
				applicationConns.Store(c, true)
				conns = append(conns, c)
			}
		}
		waitFor(t, "all stress backend streams", func() bool { return backendCount.active.Load() == int64(clients*streams) })
		sample("connected", round, false)
		abort := make(chan struct{})
		churnDone := make(chan struct{})
		go func() {
			defer close(churnDone)
			ticker := time.NewTicker(30 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-abort:
					return
				case <-ticker.C:
				}
				offline.Store(true)
				server.mu.Lock()
				for _, e := range server.sessions {
					e.session.mu.Lock()
					if e.session.active != nil {
						e.session.active.Close()
					}
					e.session.mu.Unlock()
				}
				server.mu.Unlock()
				reportDisconnects.Add(1)
				timer := time.NewTimer(10 * time.Millisecond)
				select {
				case <-abort:
					timer.Stop()
					offline.Store(false)
					return
				case <-timer.C:
				}
				offline.Store(false)
			}
		}()
		errors := make(chan error, len(conns))
		var workers sync.WaitGroup
		for i, c := range conns {
			workers.Add(1)
			go func(i int, c net.Conn) {
				defer workers.Done()
				defer c.Close()
				defer applicationConns.Delete(c)
				p := bytes.Repeat([]byte{byte(round), byte(i), byte(i >> 8), 0x5a}, (payloadBytes+3)/4)[:payloadBytes]
				for {
					if err := exchange(c, p); err != nil {
						errors <- err
						return
					}
					verifiedBytes.Add(int64(len(p)))
					if !time.Now().Before(roundEnd) {
						break
					}
					time.Sleep(250 * time.Millisecond)
				}
				if err := c.(*net.TCPConn).CloseWrite(); err != nil {
					errors <- err
					return
				}
				var b [1]byte
				if n, err := c.Read(b[:]); n != 0 || err != io.EOF {
					errors <- fmt.Errorf("extra data or missing EOF: %d %v", n, err)
				}
			}(i, c)
		}
		done := make(chan struct{})
		go func() { workers.Wait(); close(done) }()
		ticker := time.NewTicker(100 * time.Millisecond)
	run:
		for {
			select {
			case <-done:
				break run
			case <-ticker.C:
				sample("load", round, false)
			}
		}
		ticker.Stop()
		close(abort)
		<-churnDone
		for _, c := range conns {
			c.Close()
		}
		conns = nil
		close(errors)
		for err := range errors {
			t.Errorf("round %d: %v", round, err)
		}
		if t.Failed() {
			return
		}
		waitFor(t, "all transports resumed", attached)
		waitFor(t, "all stress streams released", func() bool {
			if backendCount.active.Load() != 0 || localCount.active.Load() != 0 {
				return false
			}
			server.mu.Lock()
			defer server.mu.Unlock()
			for _, e := range server.sessions {
				e.session.mu.Lock()
				clean := len(e.session.streams) == 0 && e.session.pendingBytes == 0 && e.session.pending == nil
				e.session.mu.Unlock()
				if !clean {
					return false
				}
			}
			return true
		})
		idle := sample("idle", round, true)
		if idle.Goroutines > base.Goroutines+16 {
			t.Fatalf("goroutines did not return to baseline: %d -> %d", base.Goroutines, idle.Goroutines)
		}
		if base.FileDescriptors >= 0 && idle.FileDescriptors > base.FileDescriptors+8 {
			t.Fatalf("descriptors did not return to baseline: %d -> %d", base.FileDescriptors, idle.FileDescriptors)
		}
		if idle.HeapBytes > base.HeapBytes+16*1024*1024 {
			t.Fatalf("retained heap grew beyond allowance: %d -> %d", base.HeapBytes, idle.HeapBytes)
		}
		report.CompletedRounds = round
		report.VerifiedBytesPerDirection = verifiedBytes.Load()
	}
	report.Disconnects = reportDisconnects.Load()
	cleanup()
	waitFor(t, "stress sockets closed", func() bool {
		return backendCount.active.Load() == 0 && localCount.active.Load() == 0 && httpCount.active.Load() == 0
	})
	waitFor(t, "stress goroutines stopped", func() bool { return runtime.NumGoroutine() <= initialGoroutines+4 })
	closed := sample("closed", report.CompletedRounds, true)
	if initialDescriptors >= 0 && closed.FileDescriptors > initialDescriptors+4 {
		t.Fatalf("descriptors leaked after shutdown: %d -> %d", initialDescriptors, closed.FileDescriptors)
	}
	t.Logf("completed: rounds=%d disconnects=%d bytes/direction=%d", report.CompletedRounds, report.Disconnects, report.VerifiedBytesPerDirection)
}
