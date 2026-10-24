package tunnel

import (
	"bytes"
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
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Unlike configNoiseObserver, this relay does not terminate TLS or answer
// WebSocket control frames: heartbeat evidence must come from the real peers.
func TestConfigBinaryNoiseWeakNetwork(t *testing.T) {
	cb, sb := os.Getenv("SGATE_CONFIG_CLIENT_BINARY"), os.Getenv("SGATE_CONFIG_SERVER_BINARY")
	if cb == "" || sb == "" {
		t.Skip("set SGATE_CONFIG_CLIENT_BINARY and SGATE_CONFIG_SERVER_BINARY")
	}
	baseG, baseFD := runtime.NumGoroutine(), descriptorCount()
	t.Cleanup(func() {
		waitFor(t, "weak config harness stopped", func() bool { return runtime.NumGoroutine() <= baseG+3 })
		fd := descriptorCount()
		if baseFD >= 0 && fd > baseFD+2 {
			t.Errorf("descriptors %d -> %d", baseFD, fd)
		}
		t.Logf("weak config cleanup: goroutines=%d baseline=%d descriptors=%d baseline=%d", runtime.NumGoroutine(), baseG, fd, baseFD)
	})
	for _, tlsMode := range []bool{false, true} {
		for _, mode := range []string{"file", "override", "disable"} {
			t.Run(fmt.Sprintf("tls=%t/%s", tlsMode, mode), func(t *testing.T) { t.Parallel(); runConfigBinaryWeakNetwork(t, cb, sb, tlsMode, mode) })
		}
	}
}

func runConfigBinaryWeakNetwork(t *testing.T, cb, sb string, tlsMode bool, mode string) {
	root := t.TempDir()
	keyPath := filepath.Join(root, "sgate.key")
	if err := os.WriteFile(keyPath, []byte(hex.EncodeToString(bytes.Repeat([]byte{37}, 32))), 0600); err != nil {
		t.Fatal(err)
	}
	seed := httptest.NewTLSServer(http.NotFoundHandler())
	certificate := seed.TLS.Certificates[0]
	seed.Close()
	private, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	certPath, tlsKeyPath := filepath.Join(root, "cert.pem"), filepath.Join(root, "tls.key")
	for path, block := range map[string]*pem.Block{certPath: {Type: "CERTIFICATE", Bytes: certificate.Certificate[0]}, tlsKeyPath: {Type: "PRIVATE KEY", Bytes: private}} {
		if err := os.WriteFile(path, pem.EncodeToMemory(block), 0600); err != nil {
			t.Fatal(err)
		}
	}
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var conns sync.Map
	var active, accepted atomic.Int64
	var upSent, upReceived, downSent, downReceived atomic.Int64
	results := make(chan error, 4)
	end := time.Now().Add(80 * time.Second)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, e := backend.Accept()
			if e != nil {
				return
			}
			accepted.Add(1)
			active.Add(1)
			conns.Store(c, true)
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer c.Close()
				defer conns.Delete(c)
				defer active.Add(-1)
				c.SetDeadline(time.Now().Add(110 * time.Second))
				var header [8]byte
				if _, e := io.ReadFull(c, header[:]); e != nil {
					results <- e
					return
				}
				results <- stressDuplex(c, binary.BigEndian.Uint64(header[:]), end, 512, true, &downSent, &upReceived, 100*time.Millisecond)
			}()
		}
	}()
	t.Cleanup(func() {
		backend.Close()
		conns.Range(func(k, v any) bool { k.(net.Conn).Close(); return true })
		wg.Wait()
	})
	serverAddr, clientAddr := configBinaryPort(t), configBinaryPort(t)
	relay := newWeakRelay(t, serverAddr)
	// Install impairments after startup so failures cannot be confused with a
	// startup port race. Retain actual production dial/write timeout defaults.
	common := func() map[string]string {
		return map[string]string{"key-file": keyPath, "password": testPassword, "heartbeat": "1s", "heartbeat-timeout": "18s", "buffer-bytes": "65536", "noise-interval-min": "1s", "noise-interval-max": "3s", "noise-min-bytes": "1024", "noise-max-bytes": "10240"}
	}
	sv, cv := common(), common()
	scheme := "ws"
	if tlsMode {
		scheme = "wss"
		sv["tls-cert"], sv["tls-key"], cv["tls-ca"] = certPath, tlsKeyPath, certPath
	}
	sv["listen"], sv["target"], sv["session-ttl"] = serverAddr, backend.Addr().String(), "2m"
	cv["listen"], cv["server"], cv["offline-timeout"] = clientAddr, scheme+"://"+relay.listener.Addr().String()+"/tunnel", "2m"
	sa, ca := []string{"-config", configBinaryFile(t, "server.conf", sv)}, []string{"-config", configBinaryFile(t, "client.conf", cv)}
	var flags []string
	switch mode {
	case "override":
		flags = []string{"-noise-interval-min", "100ms", "-noise-interval-max", "300ms", "-noise-min-bytes", "128", "-noise-max-bytes", "512"}
	case "disable":
		flags = []string{"-noise-interval-min", "0", "-noise-interval-max", "0"}
	}
	sa = append(sa, flags...)
	ca = append(ca, flags...)
	stopServer := configBinaryStart(t, sb, sa...)
	stopClient := configBinaryStart(t, cb, ca...)
	// A connected local port is only a readiness probe; no target stream is
	// opened until the application's first data arrives.
	var apps []net.Conn
	for i := 0; i < 2; i++ {
		var c net.Conn
		waitFor(t, "local config listener", func() bool {
			var e error
			c, e = net.DialTimeout("tcp", clientAddr, 100*time.Millisecond)
			return e == nil
		})
		apps = append(apps, c)
		t.Cleanup(func() { c.Close() })
		c.SetDeadline(time.Now().Add(110 * time.Second))
		var header [8]byte
		binary.BigEndian.PutUint64(header[:], uint64(i+700))
		if _, err := c.Write(header[:]); err != nil {
			t.Fatal(err)
		}
		go func(c net.Conn, seed uint64) {
			results <- stressDuplex(c, seed, end, 512, false, &upSent, &downReceived, 100*time.Millisecond)
		}(c, uint64(i+700))
	}
	waitFor(t, "independent traffic", func() bool { return upReceived.Load() > 0 && downReceived.Load() > 0 })
	initial := relay.accepted.Load()
	relay.profile.Store(&weakProfile{delay: 150 * time.Millisecond, jitter: 100 * time.Millisecond, lossPercent: 15, retransmit: 200 * time.Millisecond})
	timer := time.NewTimer(22 * time.Second)
	defer timer.Stop()
	<-timer.C
	if relay.accepted.Load() != initial {
		t.Fatal("healthy delayed connection falsely reconnected")
	}
	for direction := 0; direction < 2; direction++ {
		relay.blackhole[direction].Store(true)
		timer.Reset(time.Second)
		<-timer.C
		relay.blackhole[direction].Store(false)
		timer.Reset(time.Second)
		<-timer.C
		if relay.accepted.Load() != initial {
			t.Fatalf("short one-way stall %d falsely reconnected", direction)
		}
	}
	for direction := 0; direction < 2; direction++ {
		before := relay.accepted.Load()
		started := time.Now()
		relay.blackhole[direction].Store(true)
		stressWaitForBudget(t, "automatic one-way heartbeat reconnect", 28*time.Second, func() bool { return relay.accepted.Load() > before })
		relay.blackhole[direction].Store(false)
		// Restored data, not just a newly accepted socket, confirms resume.
		up, down := upReceived.Load(), downReceived.Load()
		stressWaitForBudget(t, "both directions resume", 12*time.Second, func() bool { return upReceived.Load() > up && downReceived.Load() > down })
		t.Logf("direction=%d automatic_reconnect_ms=%d transports=%d", direction, time.Since(started).Milliseconds(), relay.accepted.Load())
	}
	for i := 0; i < 4; i++ {
		select {
		case e := <-results:
			if e != nil {
				t.Fatal(e)
			}
		case <-time.After(35 * time.Second):
			t.Fatal("duplex completion timeout")
		}
	}
	if upSent.Load() == 0 || downSent.Load() == 0 || upSent.Load() != upReceived.Load() || downSent.Load() != downReceived.Load() {
		t.Fatalf("totals up=%d/%d down=%d/%d", upSent.Load(), upReceived.Load(), downSent.Load(), downReceived.Load())
	}
	if accepted.Load() != 2 {
		t.Fatalf("application streams replaced: %d", accepted.Load())
	}
	if relay.losses.Load() == 0 {
		t.Fatal("no retransmission injected")
	}
	for _, c := range apps {
		c.Close()
	}
	waitFor(t, "backend sockets released", func() bool { return active.Load() == 0 })
	stopClient()
	stopServer()
	relay.close()
	if relay.active.Load() != 0 || relay.queued.Load() != 0 {
		t.Fatalf("relay leaked active=%d queued=%d", relay.active.Load(), relay.queued.Load())
	}
	t.Logf("verified up=%d down=%d retransmissions=%d application_connections=%d active=0 queued=0", upSent.Load(), downSent.Load(), relay.losses.Load(), accepted.Load())
}

// Binary tests exercise the real config parser and flags. These matching option
// states expose private replay/stream ownership and transport timer shutdown.
func TestConfigNoiseWeakNetworkResources(t *testing.T) {
	noiseRecoveryResourceCheck(t)
	for _, tlsMode := range []string{"", "tls"} {
		for _, mode := range []string{"file", "override", "disable", "pending-hour"} {
			for _, scenario := range []struct {
				name string
				run  func(*testing.T, func(*Options), string)
			}{
				{"latency-jitter-retransmission", testWeakNetworkLatencyJitterAndRetransmission},
				{"short-one-way-stall", testWeakNetworkShortDirectionalBlackholes},
				{"one-way-resume", testWeakNetworkDirectionalOutagesResumeWithoutDuplicates},
				{"one-way-expiry", testWeakNetworkDirectionalExpiryReleasesBothSides},
			} {
				if mode == "pending-hour" && scenario.name != "one-way-expiry" {
					continue
				}
				t.Run(tlsMode+"/"+mode+"/"+scenario.name, func(t *testing.T) {
					noiseRecoveryResourceCheck(t)
					scenario.run(t, func(o *Options) {
						o.NoiseIntervalMin, o.NoiseIntervalMax = time.Second, 3*time.Second
						o.NoiseMinBytes, o.NoiseMaxBytes = 1024, 10240
						switch mode {
						case "override":
							o.NoiseIntervalMin, o.NoiseIntervalMax = 100*time.Millisecond, 300*time.Millisecond
							o.NoiseMinBytes, o.NoiseMaxBytes = 128, 512
						case "disable":
							o.NoiseIntervalMin, o.NoiseIntervalMax = 0, 0
						case "pending-hour":
							o.NoiseIntervalMin, o.NoiseIntervalMax = time.Hour, 2*time.Hour
						}
					}, tlsMode)
				})
			}
		}
	}
}
