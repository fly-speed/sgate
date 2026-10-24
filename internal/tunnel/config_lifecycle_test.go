package tunnel

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// This is intentionally opt-in: the outage cases keep networking blocked for
// 125 real seconds, beyond the configured two-minute retention window.
func TestConfigBinaryNoiseLifecycle(t *testing.T) {
	cb, sb := os.Getenv("SGATE_CONFIG_CLIENT_BINARY"), os.Getenv("SGATE_CONFIG_SERVER_BINARY")
	if cb == "" || sb == "" {
		t.Skip("set SGATE_CONFIG_CLIENT_BINARY and SGATE_CONFIG_SERVER_BINARY")
	}
	baseG, baseFD := runtime.NumGoroutine(), descriptorCount()
	t.Cleanup(func() {
		waitFor(
			t,
			"all lifecycle harness goroutines stopped",
			func() bool { return runtime.NumGoroutine() <= baseG+3 },
		)
		fd := descriptorCount()
		if baseFD >= 0 && fd > baseFD+2 {
			t.Errorf("lifecycle harness descriptors %d -> %d", baseFD, fd)
		}
		t.Logf(
			"lifecycle cleanup: goroutines=%d baseline=%d descriptors=%d baseline=%d",
			runtime.NumGoroutine(),
			baseG,
			fd,
			baseFD,
		)
	})
	for _, scenario := range []string{"long-outage", "restart", "expiry"} {
		for _, tlsMode := range []bool{false, true} {
			for _, mode := range []string{"file", "override", "disable"} {
				t.Run(
					fmt.Sprintf("%s/tls=%t/%s", scenario, tlsMode, mode),
					func(t *testing.T) {
						t.Parallel()
						runConfigBinaryLifecycle(t, cb, sb, scenario, tlsMode, mode)
					},
				)
			}
		}
	}
}

func runConfigBinaryLifecycle(t *testing.T, cb, sb, scenario string, tlsMode bool, mode string) {
	key := bytes.Repeat([]byte{27}, 32)
	root := t.TempDir()
	keyPath := filepath.Join(root, "sgate.key")
	if e := os.WriteFile(keyPath, []byte(hex.EncodeToString(key)), 0600); e != nil {
		t.Fatal(e)
	}
	seed := httptest.NewTLSServer(http.NotFoundHandler())
	certificate := seed.TLS.Certificates[0]
	roots := x509.NewCertPool()
	roots.AddCert(seed.Certificate())
	seed.Close()
	certPath, tlsKeyPath := filepath.Join(root, "cert.pem"), filepath.Join(root, "tls.key")
	private, e := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	if e != nil {
		t.Fatal(e)
	}
	for path, block := range map[string]*pem.Block{
		certPath:   {Type: "CERTIFICATE", Bytes: certificate.Certificate[0]},
		tlsKeyPath: {Type: "PRIVATE KEY", Bytes: private},
	} {
		if e := os.WriteFile(path, pem.EncodeToMemory(block), 0600); e != nil {
			t.Fatal(e)
		}
	}
	backend, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	gate := make(chan struct{})
	var gateOnce sync.Once
	release := func() { gateOnce.Do(func() { close(gate) }) }
	var backendWG sync.WaitGroup
	var backendConns sync.Map
	var oldBackendClosed, oldAppClosed, active, accepted, writers atomic.Int64
	var oldAppReleaseMS, oldBackendReleaseMS atomic.Int64
	var outageAt atomic.Int64
	errors := make(chan error, 32)
	fresh := make(chan error, 4)
	var upSent, upReceived, downSent, downReceived atomic.Int64
	checkOld := func(c net.Conn, closed *atomic.Int64, releaseMS *atomic.Int64) {
		<-gate
		done := make(chan struct{})
		go func() {
			defer close(done)
			writers.Add(1)
			_, _ = c.Write(bytes.Repeat([]byte{0xdb}, 2*1024*1024))
		}()
		n, err := io.Copy(io.Discard, c)
		// Observe the peer close before closing our endpoint to join its writer.
		elapsed := time.Since(time.Unix(0, outageAt.Load())).Milliseconds()
		for previous := releaseMS.Load(); elapsed > previous; previous = releaseMS.Load() {
			if releaseMS.CompareAndSwap(previous, elapsed) {
				break
			}
		}
		if n != 0 {
			errors <- fmt.Errorf("expired stream delivered %d stale bytes", n)
		}
		if err, ok := err.(net.Error); ok && err.Timeout() {
			errors <- fmt.Errorf("old connection timed out instead of closing: %w", err)
		}
		closed.Add(1)
		c.Close()
		<-done
	}
	backendWG.Add(1)
	go func() {
		defer backendWG.Done()
		for {
			c, e := backend.Accept()
			if e != nil {
				return
			}
			active.Add(1)
			accepted.Add(1)
			backendConns.Store(c, true)
			backendWG.Add(1)
			go func(c net.Conn) {
				defer backendWG.Done()
				defer c.Close()
				defer backendConns.Delete(c)
				defer active.Add(-1)
				c.SetDeadline(time.Now().Add(3 * time.Minute))
				_ = stressSocketBuffer(c, 16384)
				var header [9]byte
				if _, e := io.ReadFull(c, header[:]); e != nil {
					errors <- e
					return
				}
				if header[0] == 0 {
					if _, e := c.Write([]byte("ready")); e != nil {
						errors <- e
						return
					}
					checkOld(c, &oldBackendClosed, &oldBackendReleaseMS)
				} else {
					c.SetDeadline(time.Now().Add(15 * time.Second))
					fresh <- stressDuplex(
						c,
						binary.BigEndian.Uint64(header[1:]),
						time.Now().Add(time.Second),
						1024,
						true,
						&downSent,
						&upReceived,
						10*time.Millisecond,
					)
				}
			}(c)
		}
	}()
	closeBackend := func() {
		release()
		backend.Close()
		backendConns.Range(func(k, v any) bool {
			k.(net.Conn).Close()
			return true
		})
		backendWG.Wait()
	}
	t.Cleanup(closeBackend)
	serverAddr, clientAddr := configBinaryPort(t), configBinaryPort(t)
	scheme := "ws"
	if tlsMode {
		scheme = "wss"
	}
	expected := Options{
		NoiseIntervalMin: time.Second,
		NoiseIntervalMax: 3 * time.Second,
		NoiseMinBytes:    1024,
		NoiseMaxBytes:    10240,
	}
	o := &configNoiseObserver{
		pairs:        make(map[*websocket.Conn]*websocket.Conn),
		key:          key,
		target:       scheme + "://" + serverAddr + "/tunnel",
		tlsConfig:    &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		expected:     [2]Options{expected, expected},
		sessions:     make(map[string]bool),
		byGeneration: make(map[int][2]int),
	}
	for i := range o.sizes {
		o.sizes[i] = make(map[int]bool)
	}
	relay := httptest.NewUnstartedServer(o)
	if tlsMode {
		relay.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
		relay.StartTLS()
	} else {
		relay.Start()
	}
	closeRelay := func() {
		o.disconnect()
		relay.Close()
		o.wg.Wait()
		waitFor(t, "lifecycle relay handlers returned", func() bool { return o.active() == 0 })
	}
	t.Cleanup(closeRelay)
	common := func() map[string]string {
		return map[string]string{
			"key-file":           keyPath,
			"password":           testPassword,
			"heartbeat":          "1s",
			"heartbeat-timeout":  "30s",
			"buffer-bytes":       "65536",
			"noise-interval":     "0",
			"noise-interval-min": "1s",
			"noise-interval-max": "3s",
			"noise-min-bytes":    "1024",
			"noise-max-bytes":    "10240",
		}
	}
	sv, cv := common(), common()
	sv["listen"], sv["target"], sv["session-ttl"] = serverAddr, backend.Addr().String(), "2m"
	cv["listen"], cv["server"], cv["offline-timeout"] = clientAddr, strings.Replace(relay.URL, "http", "ws", 1)+"/tunnel", "2m"
	cv["reconnect-min"], cv["reconnect-max"] = "250ms", "10s"
	if scenario == "expiry" {
		sv["session-ttl"], cv["offline-timeout"] = "750ms", "30s"
	}
	if tlsMode {
		sv["tls-cert"], sv["tls-key"], cv["tls-ca"] = certPath, tlsKeyPath, certPath
	}
	sp, cp := configBinaryFile(t, "server.conf", sv), configBinaryFile(t, "client.conf", cv)
	sa, ca := []string{"-config", sp}, []string{"-config", cp}
	switch mode {
	case "override":
		flags := []string{
			"-noise-interval-min",
			"10ms",
			"-noise-interval-max",
			"30ms",
			"-noise-min-bytes",
			"128",
			"-noise-max-bytes",
			"512",
		}
		sa = append(sa, flags...)
		ca = append(ca, flags...)
		for i := range o.expected {
			o.expected[i] = Options{
				NoiseIntervalMin: 10 * time.Millisecond,
				NoiseIntervalMax: 30 * time.Millisecond,
				NoiseMinBytes:    128,
				NoiseMaxBytes:    512,
			}
		}
	case "disable":
		flags := []string{"-noise-interval-min", "0", "-noise-interval-max", "0"}
		sa = append(sa, flags...)
		ca = append(ca, flags...)
		for i := range o.expected {
			o.expected[i].NoiseIntervalMin = 0
		}
	}
	stopServer := configBinaryStart(t, sb, sa...)
	stopClient := configBinaryStart(t, cb, ca...)
	waitFor(t, "initial lifecycle transport", func() bool { return o.active() == 1 })
	warm := func() {
		waitFor(t, "lifecycle noise matches current generation", func() bool {
			o.mu.Lock()
			defer o.mu.Unlock()
			counts := o.byGeneration[o.upgrades]
			for i, v := range o.expected {
				if v.NoiseIntervalMin > 0 && counts[i] == 0 {
					return false
				}
			}
			return true
		})
	}
	warm()
	var oldSessionID string
	o.mu.Lock()
	for id := range o.sessions {
		oldSessionID = id
	}
	o.mu.Unlock()
	var apps []net.Conn
	var appWG sync.WaitGroup
	t.Cleanup(func() {
		release()
		for _, c := range apps {
			c.Close()
		}
		appWG.Wait()
	})
	for i := 0; i < 2; i++ {
		c, e := net.DialTimeout("tcp", clientAddr, time.Second)
		if e != nil {
			t.Fatal(e)
		}
		apps = append(apps, c)
		_ = stressSocketBuffer(c, 16384)
		c.SetDeadline(time.Now().Add(3 * time.Minute))
		if _, e := c.Write(make([]byte, 9)); e != nil {
			t.Fatal(e)
		}
		var ready [5]byte
		if _, e := io.ReadFull(c, ready[:]); e != nil || string(ready[:]) != "ready" {
			t.Fatalf("old stream not ready: %v", e)
		}
		appWG.Add(1)
		go func(c net.Conn) {
			defer appWG.Done()
			checkOld(c, &oldAppClosed, &oldAppReleaseMS)
		}(c)
	}
	start := time.Now()
	outageAt.Store(start.UnixNano())
	o.disconnect()
	waitFor(t, "old lifecycle transport stopped", func() bool { return o.active() == 0 })
	release()
	waitFor(t, "both directions offered unread old data", func() bool { return writers.Load() == 4 })
	switch scenario {
	case "long-outage":
		stressWaitForBudget(
			t,
			"old applications and backends released before restoration",
			125*time.Second,
			func() bool { return oldAppClosed.Load() == 2 && oldBackendClosed.Load() == 2 && active.Load() == 0 },
		)
		for _, elapsed := range []int64{oldAppReleaseMS.Load(), oldBackendReleaseMS.Load()} {
			if elapsed < 119000 || elapsed > 125000 {
				t.Fatalf("two-minute retention released outside 119..125 seconds: %dms", elapsed)
			}
		}
		if remaining := time.Until(start.Add(125 * time.Second)); remaining > 0 {
			time.Sleep(remaining)
		}
	case "restart":
		stopServer()
		waitFor(
			t,
			"stopped server backends closed",
			func() bool { return oldBackendClosed.Load() == 2 && active.Load() == 0 },
		)
		stopServer = configBinaryStart(t, sb, sa...)
		if oldAppClosed.Load() != 0 {
			t.Fatal("client abandoned retained applications before restart response")
		}
	case "expiry":
		waitFor(
			t,
			"server TTL expired while client still retained state",
			func() bool { return oldBackendClosed.Load() == 2 && active.Load() == 0 },
		)
		if oldAppClosed.Load() != 0 {
			t.Fatal("client abandoned retained applications before authenticated expiry")
		}
		if elapsed := oldBackendReleaseMS.Load(); elapsed < 650 || elapsed > 5000 {
			t.Fatalf("750ms server TTL released outside observation budget: %dms", elapsed)
		}
	}
	if !o.blocked.Load() {
		t.Fatal("old cleanup required restoring network")
	}
	blockedMS := time.Since(start).Milliseconds()
	o.blocked.Store(false)
	stressWaitForBudget(
		t,
		"fresh lifecycle session established",
		20*time.Second,
		func() bool {
			o.mu.Lock()
			defer o.mu.Unlock()
			return len(o.sessions) == 2 && len(o.pairs) == 1
		},
	)
	waitFor(t, "old applications closed after fresh session", func() bool { return oldAppClosed.Load() == 2 })
	if scenario != "long-outage" {
		o.mu.Lock()
		gone := o.statuses[http.StatusGone]
		o.mu.Unlock()
		if gone == 0 {
			t.Fatal("authenticated 410 was not observed before session replacement")
		}
	}
	warm()
	var freshSessionID string
	o.mu.Lock()
	for id := range o.sessions {
		if id != oldSessionID {
			freshSessionID = id
		}
	}
	o.mu.Unlock()
	var oldCounts [2]int
	o.mu.Lock()
	oldCounts = o.byGeneration[1]
	o.mu.Unlock()
	for i := 0; i < 2; i++ {
		c, e := net.DialTimeout("tcp", clientAddr, time.Second)
		if e != nil {
			t.Fatal(e)
		}
		apps = append(apps, c)
		c.SetDeadline(time.Now().Add(15 * time.Second))
		seed := uint64(1)<<48 | uint64(i)
		var header [9]byte
		header[0] = 1
		binary.BigEndian.PutUint64(header[1:], seed)
		if _, e := c.Write(header[:]); e != nil {
			t.Fatal(e)
		}
		go func(c net.Conn, seed uint64) {
			defer c.Close()
			fresh <- stressDuplex(
				c,
				seed,
				time.Now().Add(time.Second),
				1024,
				false,
				&upSent,
				&downReceived,
				10*time.Millisecond,
			)
		}(c, seed)
	}
	for i := 0; i < 4; i++ {
		select {
		case e := <-fresh:
			if e != nil {
				t.Fatal(e)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("fresh bidirectional verification timed out")
		}
	}
	if upSent.Load() != upReceived.Load() || downSent.Load() != downReceived.Load() {
		t.Fatal("fresh directional byte counts differ")
	}
	waitFor(t, "all backend streams released after fresh data", func() bool { return active.Load() == 0 })
	waitFor(t, "fresh session reliable frames acknowledged", func() bool {
		o.mu.Lock()
		defer o.mu.Unlock()
		p := o.wire[freshSessionID]
		return freshSessionID != "" && p != nil && p.sent[0] > 0 && p.sent[1] > 0 && p.ack[0] >= p.sent[0] &&
			p.ack[1] >= p.sent[1]
	})
	if accepted.Load() != 4 {
		t.Fatalf("unexpected backend stream count %d", accepted.Load())
	}
	o.mu.Lock()
	if o.upgrades != 2 || len(o.sessions) != 2 {
		t.Errorf("expected initial and fresh session only: upgrades=%d sessions=%d", o.upgrades, len(o.sessions))
	}
	if o.byGeneration[1] != oldCounts {
		t.Error("old transport emitted noise after replacement")
	}
	for i, v := range o.expected {
		if v.NoiseIntervalMin == 0 && o.counts[i] != 0 {
			t.Errorf("disabled direction %d sent noise", i)
		}
	}
	for _, e := range o.errors {
		t.Error(e)
	}
	t.Logf(
		("lifecycle scenario=%s old_app_ms=%d old_backend_ms=%d blocked_ms=%d " +
			"recovery_complete_ms=%d sessions=%d upgrades=%d noise=%v generations=%v upload=%d " +
			"download=%d gone=%d"),
		scenario,
		oldAppReleaseMS.Load(),
		oldBackendReleaseMS.Load(),
		blockedMS,
		time.Since(start).Milliseconds(),
		len(o.sessions),
		o.upgrades,
		o.counts,
		o.byGeneration,
		upReceived.Load(),
		downReceived.Load(),
		o.statuses[http.StatusGone],
	)
	o.mu.Unlock()
	for len(errors) > 0 {
		t.Error(<-errors)
	}
	stopClient()
	stopServer()
	closeRelay()
	closeBackend()
	appWG.Wait()
}

func TestConfigNoiseLifecycleResources(t *testing.T) {
	for _, tlsMode := range []string{"", "tls"} {
		for _, mode := range []string{"file", "override", "disable", "pending-hour"} {
			for _, scenario := range []struct {
				name string
				run  func(*testing.T, Options, string)
			}{
				{"long-outage", testLongOutageReleasesStateBeforeNetworkReturns},
				{"restart", testServerRestartDropsOldQueuesAndAcceptsFreshConnections},
				{"expiry", testSessionExpiryWithFullBuffersReleasesOldConnections},
			} {
				t.Run(tlsMode+"/"+mode+"/"+scenario.name, func(t *testing.T) {
					noiseRecoveryResourceCheck(t)
					opts := testOptions()
					opts.NoiseIntervalMin, opts.NoiseIntervalMax = 20*time.Millisecond, 60*time.Millisecond
					opts.NoiseMinBytes, opts.NoiseMaxBytes = 1024, 10240
					switch mode {
					case "override":
						opts.NoiseIntervalMin, opts.NoiseIntervalMax = 10*time.Millisecond, 30*time.Millisecond
						opts.NoiseMinBytes, opts.NoiseMaxBytes = 128, 512
					case "disable":
						opts.NoiseIntervalMin, opts.NoiseIntervalMax = 0, 0
					case "pending-hour":
						opts.NoiseIntervalMin, opts.NoiseIntervalMax = time.Hour, 2*time.Hour
					}
					scenario.run(t, opts, tlsMode)
				})
			}
		}
	}
}

func TestConfigObserverPreservesExpiryResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Sgate-Nonce", "expiry-nonce")
		w.Header().Set("X-Sgate-Auth", "expiry-auth")
		http.Error(w, "session expired", http.StatusGone)
	}))
	defer upstream.Close()
	o := &configNoiseObserver{target: strings.Replace(upstream.URL, "http", "ws", 1)}
	relay := httptest.NewServer(o)
	defer relay.Close()
	conn, response, err := (&websocket.Dialer{}).Dial(strings.Replace(relay.URL, "http", "ws", 1), nil)
	if conn != nil {
		conn.Close()
	}
	if err == nil || response == nil {
		t.Fatal("expected failed upgrade with original expiry response")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusGone || response.Header.Get("X-Sgate-Nonce") != "expiry-nonce" ||
		response.Header.Get("X-Sgate-Auth") != "expiry-auth" {
		t.Fatal("observer hid the authenticated expiry response")
	}
	body, e := io.ReadAll(response.Body)
	if e != nil || !strings.Contains(string(body), "session expired") {
		t.Fatalf("expiry body not preserved: %v", e)
	}
}
