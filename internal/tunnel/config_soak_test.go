package tunnel

import (
	"bytes"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
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
)

type configSoakSample struct {
	Phase         string           `json:"phase"`
	UnixNano      int64            `json:"unix_nano"`
	PID           int              `json:"pid"`
	HeapBytes     uint64           `json:"heap_bytes"`
	RSSBytes      int64            `json:"rss_bytes"`
	Goroutines    int              `json:"goroutines"`
	FDs           int              `json:"fds"`
	Writers       int              `json:"transport_writers"`
	Truncated     bool             `json:"stack_truncated"`
	Census        map[string]int64 `json:"census"`
	NoiseMin      string           `json:"noise_min"`
	NoiseMax      string           `json:"noise_max"`
	NoiseMinBytes string           `json:"noise_min_bytes"`
	NoiseMaxBytes string           `json:"noise_max_bytes"`
}

func configSoakSamples(t *testing.T, path string) []configSoakSample {
	t.Helper()
	b, e := os.ReadFile(path + ".metrics")
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		t.Fatal(e)
	}
	lines := bytes.Split(b, []byte{'\n'})
	var out []configSoakSample
	// An in-progress final append is retried on the next poll.
	for _, line := range lines[:len(lines)-1] {
		var v configSoakSample
		if e := json.Unmarshal(line, &v); e != nil {
			t.Fatal(e)
		}
		if v.Truncated {
			t.Fatal("metrics stack census truncated")
		}
		out = append(out, v)
	}
	return out
}
func configSoakCollect(t *testing.T, path, phase string) configSoakSample {
	t.Helper()
	if e := os.WriteFile(path+".collect", []byte(phase), 0600); e != nil {
		t.Fatal(e)
	}
	var got configSoakSample
	stressWaitForBudget(t, "native metrics "+phase, 12*time.Second, func() bool {
		for _, v := range configSoakSamples(t, path) {
			if v.Phase == phase {
				got = v
				return true
			}
		}
		return false
	})
	return got
}
func TestConfigBinaryNoiseSoak(t *testing.T) {
	if os.Getenv("SGATE_CONFIG_SOAK") != "1" {
		t.Skip("set SGATE_CONFIG_SOAK=1 and diagnostic binary paths")
	}
	cb, sb := os.Getenv("SGATE_CONFIG_CLIENT_BINARY"), os.Getenv("SGATE_CONFIG_SERVER_BINARY")
	if cb == "" || sb == "" {
		t.Fatal("diagnostic binaries required")
	}
	baseG, baseFD := runtime.NumGoroutine(), descriptorCount()
	t.Cleanup(func() {
		waitFor(t, "soak harness goroutines released", func() bool { return runtime.NumGoroutine() <= baseG+4 })
		if fd := descriptorCount(); baseFD >= 0 && fd > baseFD+2 {
			t.Errorf("soak descriptors %d -> %d", baseFD, fd)
		}
		t.Logf(
			"soak cleanup goroutines=%d/%d descriptors=%d/%d",
			runtime.NumGoroutine(),
			baseG,
			descriptorCount(),
			baseFD,
		)
	})
	for _, tlsMode := range []bool{false, true} {
		for _, mode := range []string{"file", "override", "disable"} {
			t.Run(
				fmt.Sprintf("tls=%t/%s", tlsMode, mode),
				func(t *testing.T) {
					t.Parallel()
					runConfigBinarySoak(t, cb, sb, tlsMode, mode)
				},
			)
		}
	}
}
func runConfigBinarySoak(t *testing.T, cb, sb string, tlsMode bool, mode string) {
	streams, rounds, duration := 128, 4, 75*time.Second
	if os.Getenv("SGATE_CONFIG_SOAK_SMOKE") == "1" {
		streams, rounds, duration = 4, 2, 10*time.Second
	}
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
	for path, block := range map[string]*pem.Block{
		certPath:   {Type: "CERTIFICATE", Bytes: certificate.Certificate[0]},
		tlsKeyPath: {Type: "PRIVATE KEY", Bytes: private},
	} {
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
	results := make(chan error, 2*streams)
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
				c.SetDeadline(time.Now().Add(duration + time.Minute))
				var header [16]byte
				if _, e := io.ReadFull(c, header[:]); e != nil {
					results <- e
					return
				}
				end := time.Unix(0, int64(binary.BigEndian.Uint64(header[8:])))
				c.SetDeadline(end.Add(45 * time.Second))
				results <- stressDuplex(
					c,
					binary.BigEndian.Uint64(header[:8]),
					end,
					256,
					true,
					&downSent,
					&upReceived,
					time.Second,
				)
			}()
		}
	}()
	t.Cleanup(func() {
		backend.Close()
		conns.Range(func(k, v any) bool {
			k.(net.Conn).Close()
			return true
		})
		wg.Wait()
	})
	serverAddr, clientAddr := configBinaryPort(t), configBinaryPort(t)
	relay := newWeakRelay(t, serverAddr)
	// Install impairments after startup so failures cannot be confused with a
	// startup port race. Retain actual production dial/write timeout defaults.
	common := func() map[string]string {
		return map[string]string{
			"key-file":           keyPath,
			"password":           testPassword,
			"heartbeat":          "1s",
			"heartbeat-timeout":  "18s",
			"buffer-bytes":       "262144",
			"max-streams":        "160",
			"noise-interval-min": "1s",
			"noise-interval-max": "3s",
			"noise-min-bytes":    "1024",
			"noise-max-bytes":    "10240",
		}
	}
	sv, cv := common(), common()
	scheme := "ws"
	if tlsMode {
		scheme = "wss"
		sv["tls-cert"], sv["tls-key"], cv["tls-ca"] = certPath, tlsKeyPath, certPath
	}
	sv["listen"], sv["target"], sv["session-ttl"] = serverAddr, backend.Addr().String(), "2m"
	cv["listen"], cv["server"], cv["offline-timeout"] = clientAddr, scheme+"://"+relay.listener.Addr().String()+"/tunnel", "2m"
	sa, ca := []string{"-config", configBinaryFile(t, "server.conf", sv)}, []string{
		"-config",
		configBinaryFile(
			t,
			"client.conf",
			cv,
		),
	}
	var flags []string
	switch mode {
	case "override":
		flags = []string{
			"-noise-interval-min",
			"100ms",
			"-noise-interval-max",
			"300ms",
			"-noise-min-bytes",
			"128",
			"-noise-max-bytes",
			"512",
		}
	case "disable":
		flags = []string{"-noise-interval-min", "0", "-noise-interval-max", "0"}
	}
	sa = append(sa, flags...)
	ca = append(ca, flags...)

	stopServer := configBinaryStart(t, sb, sa...)
	stopClient := configBinaryStart(t, cb, ca...)
	// Preserve telemetry even on failure; configuration files contain disposable secrets.
	t.Cleanup(func() {
		if dir := os.Getenv("SGATE_CONFIG_SOAK_REPORT_DIR"); dir != "" {
			if e := os.MkdirAll(dir, 0700); e != nil {
				t.Error(e)
				return
			}
			for role, path := range map[string]string{"client": ca[1], "server": sa[1]} {
				if b, e := os.ReadFile(path + ".metrics"); e == nil {
					if e = os.WriteFile(filepath.Join(dir, strings.ReplaceAll(t.Name(), "/", "_")+"-"+role+".jsonl"), b, 0600); e != nil {
						t.Error(e)
					}
				}
			}
		}
	})
	waitFor(t, "native session established", func() bool { return relay.active.Load() == 1 })
	var baseline [2]configSoakSample
	paths := []string{ca[1], sa[1]}
	for i, path := range paths {
		baseline[i] = configSoakCollect(t, path, "baseline")
		if baseline[i].Writers != 1 || baseline[i].Census["sessions"] != 1 || baseline[i].FDs < 0 {
			t.Fatalf("baseline not attached: %+v", baseline[i])
		}
	}
	relay.profile.Store(&weakProfile{
		delay:       150 * time.Millisecond,
		jitter:      100 * time.Millisecond,
		lossPercent: 15,
		retransmit:  200 * time.Millisecond,
	})
	var drops atomic.Int64
	var firstDrained [2]configSoakSample
	for round := 1; round <= rounds; round++ {
		end := time.Now().Add(duration)
		var apps []net.Conn
		closeApps := func() {
			for _, c := range apps {
				c.Close()
			}
		}
		t.Cleanup(closeApps)
		for i := 0; i < streams; i++ {
			c, e := net.DialTimeout("tcp", clientAddr, 3*time.Second)
			if e != nil {
				t.Fatal(e)
			}
			apps = append(apps, c)
			c.SetDeadline(end.Add(45 * time.Second))
			seed := uint64(round)<<32 | uint64(i)
			var hello [16]byte
			binary.BigEndian.PutUint64(hello[:8], seed)
			binary.BigEndian.PutUint64(hello[8:], uint64(end.UnixNano()))
			if _, e := c.Write(hello[:]); e != nil {
				t.Fatal(e)
			}
			go func(c net.Conn, seed uint64) {
				results <- stressDuplex(c, seed, end, 256, false, &upSent, &downReceived, time.Second)
			}(c, seed)
		}
		stressWaitForBudget(
			t,
			"full concurrent backend cohort",
			15*time.Second,
			func() bool { return active.Load() == int64(streams) },
		)
		done, joined := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(joined)
			tick := time.NewTicker(6 * time.Second)
			defer tick.Stop()
			for {
				select {
				case <-done:
					return
				case <-tick.C:
					relay.mu.Lock()
					for p := range relay.pairs {
						p.close()
					}
					relay.mu.Unlock()
					drops.Add(1)
				}
			}
		}()
		var once sync.Once
		stopChurn := func() {
			once.Do(func() {
				close(done)
				<-joined
			})
		}
		t.Cleanup(stopChurn)
		timer := time.NewTimer(time.Until(end))
		<-timer.C
		stopChurn()
		for n := 0; n < 2*streams; n++ {
			select {
			case e := <-results:
				if e != nil {
					t.Fatal(e)
				}
			case <-time.After(50 * time.Second):
				t.Fatal("cohort duplex completion timed out")
			}
		}
		closeApps()
		waitFor(t, "cohort backend sockets released", func() bool { return active.Load() == 0 })
		if accepted.Load() != int64(round*streams) {
			t.Fatalf("backend streams replaced after resume: %d", accepted.Load())
		}
		stressWaitForBudget(t, "both native replay queues drained", 15*time.Second, func() bool {
			for _, path := range paths {
				samples := configSoakSamples(t, path)
				if len(samples) == 0 {
					return false
				}
				v := samples[len(samples)-1]
				if v.Census["streams"] != 0 || v.Census["pending_bytes"] != 0 || v.Writers != 1 {
					return false
				}
			}
			return true
		})
		for i, path := range paths {
			v := configSoakCollect(t, path, fmt.Sprintf("drained-%d", round))
			if v.Census["streams"] != 0 || v.Census["pending_bytes"] != 0 || v.Census["sessions"] != 1 ||
				v.Writers != 1 {
				t.Fatalf("native drained resource leak: %+v", v)
			}
			if v.Goroutines > baseline[i].Goroutines+12 || v.FDs < 0 || v.FDs > baseline[i].FDs+4 {
				t.Fatalf("native resource growth baseline=%+v now=%+v", baseline[i], v)
			}
			if v.Census["nonces"] > 64 {
				t.Fatalf("nonce cache exceeded retention bound: %+v", v)
			}
			if round == 1 {
				firstDrained[i] = v
			} else if v.HeapBytes > firstDrained[i].HeapBytes+(8<<20) {
				t.Fatalf("post-GC heap growth >8MiB: first=%+v now=%+v", firstDrained[i], v)
			}
			expectedMin, expectedMax, minBytes, maxBytes := "1s", "3s", "1024", "10240"
			switch mode {
			case "override":
				expectedMin, expectedMax, minBytes, maxBytes = "100ms", "300ms", "128", "512"
			case "disable":
				expectedMin, expectedMax = "0s", "0s"
			}
			if v.NoiseMin != expectedMin || v.NoiseMax != expectedMax || v.NoiseMinBytes != minBytes ||
				v.NoiseMaxBytes != maxBytes {
				t.Fatalf("native flag priority mismatch: %+v", v)
			}
			if mode == "disable" && v.Census["noise_sent"] != 0 {
				t.Fatalf("disabled peer sent noise: %+v", v)
			}
			if mode != "disable" && (v.Census["noise_sent"] == 0 || v.Census["noise_received"] == 0) {
				t.Fatalf("enabled peer did not exchange noise: %+v", v)
			}
			t.Logf(
				"round=%d role=%d heap=%d rss=%d g=%d fd=%d writers=%d census=%v",
				round,
				i,
				v.HeapBytes,
				v.RSSBytes,
				v.Goroutines,
				v.FDs,
				v.Writers,
				v.Census,
			)
		}
	}
	if upSent.Load() == 0 || downSent.Load() == 0 || upSent.Load() != upReceived.Load() ||
		downSent.Load() != downReceived.Load() {
		t.Fatalf(
			"duplex counts mismatch up=%d/%d down=%d/%d",
			upSent.Load(),
			upReceived.Load(),
			downSent.Load(),
			downReceived.Load(),
		)
	}
	if drops.Load() < int64(rounds) || relay.losses.Load() == 0 {
		t.Fatal("missing churn or retransmission")
	}
	stopClient()
	stopServer()
	relay.close()
	for _, path := range paths {
		samples := configSoakSamples(t, path)
		v := samples[len(samples)-1]
		if v.Phase != "closed" || v.Writers != 0 || v.Census["sessions"] != 0 || v.Census["streams"] != 0 ||
			v.Census["pending_bytes"] != 0 ||
			v.Census["nonces"] != 0 {
			t.Fatalf("native shutdown leak: %+v", v)
		}
	}
	if relay.active.Load() != 0 || relay.queued.Load() != 0 {
		t.Fatal("soak relay resources leaked")
	}
	t.Logf(
		("soak verified rounds=%d streams=%d upload=%d download=%d drops=%d relay_accepted=%d " +
			"retransmissions=%d"),
		rounds,
		streams,
		upSent.Load(),
		downSent.Load(),
		drops.Load(),
		relay.accepted.Load(),
		relay.losses.Load(),
	)
}
