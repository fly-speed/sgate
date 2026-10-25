package tunnel

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Opt-in process-level coverage uses the real flag/config entrypoints. The
// observer forwards encrypted frames unchanged; its independent codecs only
// inspect noise sizes and cumulative ACKs using disposable test credentials.
type configNoiseObserver struct {
	onFrame      func(direction int, sessionID string, f frame)
	mu           sync.Mutex
	pairs        map[*websocket.Conn]*websocket.Conn
	blocked      atomic.Bool
	wg           sync.WaitGroup
	key          []byte
	target       string
	tlsConfig    *tls.Config
	expected     [2]Options
	counts       [2]int
	sizes        [2]map[int]bool
	sent, ack    [2]uint64
	sessions     map[string]bool
	upgrades     int
	byGeneration map[int][2]int
	errors       []error
	statuses     map[int]int
	wire         map[string]*configSessionProgress
}

type configSessionProgress struct{ sent, ack [2]uint64 }

func (o *configNoiseObserver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if o.blocked.Load() {
		http.Error(w, "offline", 503)
		return
	}
	headers := http.Header{}
	for k, v := range r.Header {
		if strings.HasPrefix(http.CanonicalHeaderKey(k), "X-Sgate-") {
			headers[k] = v
		}
	}
	// 原始查询串属于认证内容，不能在观测中继中丢弃窗口或后端地址。
	target, err := url.Parse(o.target)
	if err != nil {
		http.Error(w, "invalid observer upstream", 500)
		return
	}
	target.RawQuery, target.ForceQuery = r.URL.RawQuery, r.URL.ForceQuery
	up, response, err := (&websocket.Dialer{TLSClientConfig: o.tlsConfig, HandshakeTimeout: time.Second}).Dial(
		target.String(),
		headers,
	)
	if err != nil {
		if response != nil {
			defer response.Body.Close()
			for k, v := range response.Header {
				if strings.HasPrefix(http.CanonicalHeaderKey(k), "X-Sgate-") {
					w.Header()[k] = v
				}
			}
			o.mu.Lock()
			if o.statuses == nil {
				o.statuses = make(map[int]int)
			}
			o.statuses[response.StatusCode]++
			o.mu.Unlock()
			w.WriteHeader(response.StatusCode)
			_, _ = io.Copy(w, io.LimitReader(response.Body, 16*1024))
			return
		}
		http.Error(w, "upstream unavailable", 503)
		return
	}
	responseHeaders := http.Header{}
	for k, v := range response.Header {
		if strings.HasPrefix(http.CanonicalHeaderKey(k), "X-Sgate-") {
			responseHeaders[k] = v
		}
	}
	down, err := (&websocket.Upgrader{}).Upgrade(w, r, responseHeaders)
	if err != nil {
		up.Close()
		return
	}
	defer down.Close()
	defer up.Close()
	o.mu.Lock()
	if o.blocked.Load() {
		o.mu.Unlock()
		return
	}
	o.pairs[down] = up
	o.upgrades++
	generation := o.upgrades
	o.sessions[r.Header.Get("X-Sgate-Session")] = true
	o.wg.Add(2)
	o.mu.Unlock()
	defer func() {
		o.mu.Lock()
		delete(o.pairs, down)
		o.mu.Unlock()
	}()
	// Masking is handled by Gorilla. Ciphertext, crypto counters and application
	// frame ordering are unchanged; control replies belong to each relay leg.
	done := make(chan struct{}, 2)
	for direction := 0; direction < 2; direction++ {
		src, dst := down, up
		if direction == 1 {
			src, dst = up, down
		}
		crypt := newCodec(
			o.key,
			r.Header.Get("X-Sgate-Session"),
			r.Header.Get("X-Sgate-Nonce"),
			response.Header.Get("X-Sgate-Nonce"),
			direction == 1,
		)
		go func(direction int, src, dst *websocket.Conn, crypt *codec) {
			defer o.wg.Done()
			defer func() {
				src.Close()
				dst.Close()
				done <- struct{}{}
			}()
			for {
				typ, data, err := src.ReadMessage()
				if err != nil {
					return
				}
				plain, err := crypt.open(data)
				if err == nil {
					var f frame
					f, err = decodeFrame(plain)
					if err == nil {
						o.observe(direction, generation, r.Header.Get("X-Sgate-Session"), f)
					}
				}
				if err != nil {
					o.mu.Lock()
					o.errors = append(o.errors, err)
					o.mu.Unlock()
					return
				}
				if err := dst.WriteMessage(typ, data); err != nil {
					return
				}
			}
		}(direction, src, dst, crypt)
	}
	<-done
	<-done
}

func (o *configNoiseObserver) observe(direction, generation int, sessionID string, f frame) {
	if o.onFrame != nil {
		o.onFrame(direction, sessionID, f)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if f.op == opNoise {
		opts := o.expected[direction]
		if opts.NoiseIntervalMin == 0 || len(f.data) < opts.NoiseMinBytes || len(f.data) > opts.NoiseMaxBytes {
			o.errors = append(o.errors, fmt.Errorf("direction %d unexpected noise size %d", direction, len(f.data)))
		}
		o.counts[direction]++
		counts := o.byGeneration[generation]
		counts[direction]++
		o.byGeneration[generation] = counts
		o.sizes[direction][len(f.data)] = true
	} else {
		o.sent[direction] = max(o.sent[direction], f.seq)
		o.ack[1-direction] = max(o.ack[1-direction], f.ack)
		if o.wire == nil {
			o.wire = make(map[string]*configSessionProgress)
		}
		p := o.wire[sessionID]
		if p == nil {
			p = &configSessionProgress{}
			o.wire[sessionID] = p
		}
		p.sent[direction] = max(p.sent[direction], f.seq)
		p.ack[1-direction] = max(p.ack[1-direction], f.ack)
	}
}
func (o *configNoiseObserver) disconnect() {
	o.blocked.Store(true)
	o.mu.Lock()
	defer o.mu.Unlock()
	for a, b := range o.pairs {
		a.Close()
		b.Close()
	}
}
func (o *configNoiseObserver) active() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.pairs)
}

func configBinaryStart(t *testing.T, binary string, args ...string) func() {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "process.log")
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, args...)
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		log.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = cmd.Process.Signal(os.Interrupt)
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("proxy exit: %v", err)
				}
			case <-time.After(8 * time.Second):
				cmd.Process.Kill()
				<-done
				t.Error("proxy shutdown timed out")
			}
			log.Close()
			if t.Failed() {
				if b, e := os.ReadFile(logPath); e == nil {
					t.Logf("proxy log:\n%s", b)
				}
			}
		})
	}
	t.Cleanup(stop)
	return stop
}
func configBinaryPort(t *testing.T) string {
	t.Helper()
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	a := l.Addr().String()
	l.Close()
	return a
}
func configBinaryFile(t *testing.T, name string, values map[string]string) string {
	t.Helper()
	var b strings.Builder
	for k, v := range values {
		fmt.Fprintf(&b, "%s = %q\n", k, v)
	}
	p := filepath.Join(t.TempDir(), name)
	if e := os.WriteFile(p, []byte(b.String()), 0600); e != nil {
		t.Fatal(e)
	}
	return p
}

func TestConfigBinaryNoiseReconnect(t *testing.T) {
	cb, sb := os.Getenv("SGATE_CONFIG_CLIENT_BINARY"), os.Getenv("SGATE_CONFIG_SERVER_BINARY")
	if cb == "" || sb == "" {
		t.Skip("set SGATE_CONFIG_CLIENT_BINARY and SGATE_CONFIG_SERVER_BINARY to matching builds")
	}
	for _, tlsMode := range []bool{false, true} {
		for _, mode := range []string{
			"file",
			"override",
			"disable-client",
			"disable-server",
			"disable-both",
			"file-production",
		} {
			t.Run(fmt.Sprintf("tls=%t/%s", tlsMode, mode), func(t *testing.T) {
				noiseRecoveryResourceCheck(t)
				trafficDuration := 2 * time.Second
				if mode == "file-production" {
					trafficDuration = 14 * time.Second
				}
				key := bytes.Repeat([]byte{27}, 32)
				keyPath := filepath.Join(t.TempDir(), "sgate.key")
				if e := os.WriteFile(keyPath, []byte(hex.EncodeToString(key)), 0600); e != nil {
					t.Fatal(e)
				}
				root := t.TempDir()
				certSeed := httptest.NewTLSServer(http.NotFoundHandler())
				certificate := certSeed.TLS.Certificates[0]
				roots := x509.NewCertPool()
				roots.AddCert(certSeed.Certificate())
				certSeed.Close()
				certPath, keyTLSPath := filepath.Join(root, "cert.pem"), filepath.Join(root, "key.pem")
				private, e := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
				if e != nil {
					t.Fatal(e)
				}
				if e = os.WriteFile(
					certPath,
					pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]}),
					0600,
				); e != nil {
					t.Fatal(e)
				}
				if e = os.WriteFile(keyTLSPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}), 0600); e != nil {
					t.Fatal(e)
				}
				backend, e := net.Listen("tcp", "127.0.0.1:0")
				if e != nil {
					t.Fatal(e)
				}
				var backendWG sync.WaitGroup
				var backendConns sync.Map
				var accepted atomic.Int64
				results := make(chan error, 8)
				var upSent, upReceived, downSent, downReceived atomic.Int64
				backendWG.Add(1)
				go func() {
					defer backendWG.Done()
					for {
						c, e := backend.Accept()
						if e != nil {
							return
						}
						backendConns.Store(c, true)
						i := accepted.Add(1) - 1
						backendWG.Add(1)
						go func(c net.Conn, i int64) {
							defer backendWG.Done()
							defer c.Close()
							defer backendConns.Delete(c)
							c.SetDeadline(time.Now().Add(trafficDuration + 15*time.Second))
							results <- stressDuplex(
								c,
								uint64(i),
								time.Now().Add(trafficDuration),
								1024,
								true,
								&downSent,
								&upReceived,
								20*time.Millisecond,
							)
						}(c, i)
					}
				}()
				cleanupBackend := func() {
					backend.Close()
					backendConns.Range(func(k, v any) bool {
						k.(net.Conn).Close()
						return true
					})
					backendWG.Wait()
				}
				t.Cleanup(cleanupBackend)
				serverAddr, clientAddr := configBinaryPort(t), configBinaryPort(t)
				scheme := "ws"
				if tlsMode {
					scheme = "wss"
				}
				expected := Options{
					NoiseIntervalMin: 20 * time.Millisecond,
					NoiseIntervalMax: 60 * time.Millisecond,
					NoiseMinBytes:    1024,
					NoiseMaxBytes:    4096,
				}
				observer := &configNoiseObserver{
					pairs:        make(map[*websocket.Conn]*websocket.Conn),
					key:          key,
					target:       scheme + "://" + serverAddr + "/tunnel",
					tlsConfig:    &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
					expected:     [2]Options{expected, expected},
					sessions:     make(map[string]bool),
					byGeneration: make(map[int][2]int),
				}
				for i := range observer.sizes {
					observer.sizes[i] = make(map[int]bool)
				}
				relay := httptest.NewUnstartedServer(observer)
				if tlsMode {
					relay.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
					relay.StartTLS()
				} else {
					relay.Start()
				}
				closeRelay := func() {
					observer.disconnect()
					relay.Close()
					observer.wg.Wait()
				}
				t.Cleanup(closeRelay)
				common := func() map[string]string {
					return map[string]string{
						"key-file":           keyPath,
						"password":           testPassword,
						"heartbeat":          "1s",
						"heartbeat-timeout":  "30s",
						"noise-interval":     "0",
						"noise-interval-min": "20ms",
						"noise-interval-max": "60ms",
						"noise-min-bytes":    "1024",
						"noise-max-bytes":    "4096",
					}
				}
				sv, cv := common(), common()
				sv["listen"], sv["target"] = serverAddr, backend.Addr().String()
				cv["listen"], cv["server"] = clientAddr, strings.Replace(relay.URL, "http", "ws", 1)+"/tunnel"
				cv["reconnect-min"], cv["reconnect-max"] = "20ms", "100ms"
				if tlsMode {
					sv["tls-cert"], sv["tls-key"], cv["tls-ca"] = certPath, keyTLSPath, certPath
				}
				if mode == "file-production" {
					for _, v := range []map[string]string{sv, cv} {
						v["noise-interval-min"], v["noise-interval-max"], v["noise-max-bytes"] = "1s", "3s", "10240"
					}
					for i := range observer.expected {
						observer.expected[i] = Options{
							NoiseIntervalMin: time.Second,
							NoiseIntervalMax: 3 * time.Second,
							NoiseMinBytes:    1024,
							NoiseMaxBytes:    10240,
						}
					}
				}
				sp, cp := configBinaryFile(t, "server.conf", sv), configBinaryFile(t, "client.conf", cv)
				sa, ca := []string{"-config", sp}, []string{"-config", cp}
				switch mode {
				case "override":
					flags := []string{
						"-noise-interval-min",
						"5ms",
						"-noise-interval-max",
						"15ms",
						"-noise-min-bytes",
						"128",
						"-noise-max-bytes",
						"512",
					}
					sa = append(sa, flags...)
					ca = append(ca, flags...)
					for i := range observer.expected {
						observer.expected[i] = Options{
							NoiseIntervalMin: 5 * time.Millisecond,
							NoiseIntervalMax: 15 * time.Millisecond,
							NoiseMinBytes:    128,
							NoiseMaxBytes:    512,
						}
					}
				case "disable-client", "disable-server", "disable-both":
					flags := []string{"-noise-interval-min", "0", "-noise-interval-max", "0"}
					if mode != "disable-server" {
						ca = append(ca, flags...)
						observer.expected[0].NoiseIntervalMin = 0
					}
					if mode != "disable-client" {
						sa = append(sa, flags...)
						observer.expected[1].NoiseIntervalMin = 0
					}
				}
				stopServer := configBinaryStart(t, sb, sa...)
				stopClient := configBinaryStart(t, cb, ca...)
				waitFor(t, "one configured transport", func() bool { return observer.active() == 1 })
				warm := func() {
					stressWaitForBudget(t, "configured noise emitted", 10*time.Second, func() bool {
						observer.mu.Lock()
						defer observer.mu.Unlock()
						for i, o := range observer.expected {
							if o.NoiseIntervalMin > 0 && (observer.counts[i] < 2 || len(observer.sizes[i]) < 2) {
								return false
							}
						}
						return true
					})
				}
				warm()
				var apps []net.Conn
				defer func() {
					for _, c := range apps {
						c.Close()
					}
				}()
				for i := 0; i < 4; i++ {
					c, e := net.DialTimeout("tcp", clientAddr, time.Second)
					if e != nil {
						t.Fatal(e)
					}
					c.SetDeadline(time.Now().Add(trafficDuration + 15*time.Second))
					apps = append(apps, c)
					// Ensure the unique verifier seed follows backend acceptance order.
					waitFor(t, "backend accepted stream", func() bool { return accepted.Load() == int64(i+1) })
				}
				for i, c := range apps {
					go func(i int, c net.Conn) {
						results <- stressDuplex(
							c,
							uint64(i),
							time.Now().Add(trafficDuration),
							1024,
							false,
							&upSent,
							&downReceived,
							20*time.Millisecond,
						)
					}(i, c)
				}
				for n := 0; n < 3; n++ {
					time.Sleep(300 * time.Millisecond)
					observer.disconnect()
					waitFor(t, "old relay transports closed", func() bool { return observer.active() == 0 })
					time.Sleep(100 * time.Millisecond)
					observer.blocked.Store(false)
					waitFor(t, "configured reconnect", func() bool { return observer.active() == 1 })
					waitFor(t, "noise restarted on replacement transport", func() bool {
						observer.mu.Lock()
						defer observer.mu.Unlock()
						counts := observer.byGeneration[observer.upgrades]
						for i, opts := range observer.expected {
							if opts.NoiseIntervalMin > 0 && counts[i] == 0 {
								return false
							}
						}
						return true
					})
				}
				for n := 0; n < 8; n++ {
					select {
					case e := <-results:
						if e != nil {
							t.Fatal(e)
						}
					case <-time.After(15 * time.Second):
						t.Fatal("duplex verifier did not finish")
					}
				}
				if upSent.Load() != upReceived.Load() || downSent.Load() != downReceived.Load() {
					t.Fatal("directional byte counts differ")
				}
				warm()
				waitFor(t, "all reliable frames acknowledged", func() bool {
					observer.mu.Lock()
					defer observer.mu.Unlock()
					return observer.ack[0] >= observer.sent[0] && observer.ack[1] >= observer.sent[1]
				})
				if accepted.Load() != 4 {
					t.Fatal("reconnect replaced original backend TCP streams")
				}
				observer.mu.Lock()
				if observer.upgrades != 4 || len(observer.sessions) != 1 {
					t.Errorf("upgrades=%d distinct sessions=%d", observer.upgrades, len(observer.sessions))
				}
				for i, o := range observer.expected {
					if o.NoiseIntervalMin == 0 && observer.counts[i] != 0 {
						t.Errorf("disabled direction %d sent noise", i)
					}
				}
				for _, e := range observer.errors {
					t.Error(e)
				}
				t.Logf(
					("noise up/down=%v sizes=%d/%d upgrades=%d upload=%d download=%d ACK=%v seq=%v " +
						"generations=%v"),
					observer.counts,
					len(observer.sizes[0]),
					len(observer.sizes[1]),
					observer.upgrades,
					upReceived.Load(),
					downReceived.Load(),
					observer.ack,
					observer.sent,
					observer.byGeneration,
				)
				observer.mu.Unlock()
				stopClient()
				stopServer()
				closeRelay()
				cleanupBackend()
				if observer.active() != 0 {
					t.Fatal("relay sockets survived shutdown")
				}
			})
		}
	}
}
