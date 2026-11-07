package app

import (
	"bytes"
	"crypto/x509"
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

	"sgate/internal/admin"
)

type managementAPI struct {
	h      *admin.Handler
	cookie *http.Cookie
	csrf   string
}

func (a *managementAPI) call(method, path string, input any) *httptest.ResponseRecorder {
	var b []byte
	if input != nil {
		b, _ = json.Marshal(input)
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	if a.cookie != nil {
		r.AddCookie(a.cookie)
		r.Header.Set("X-CSRF-Token", a.csrf)
	}
	w := httptest.NewRecorder()
	a.h.ServeHTTP(w, r)
	return w
}
func apiObject(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var v map[string]any
	if e := json.Unmarshal(w.Body.Bytes(), &v); e != nil {
		t.Fatal(e)
	}
	return v
}
func managementFor(t *testing.T, c *controller) *managementAPI {
	t.Helper()
	dir := t.TempDir()
	webDir, e := filepath.Abs("../../web/admin")
	if e != nil {
		t.Fatal(e)
	}
	cfg := admin.Config{
		WebDir:         webDir,
		Listen:         "127.0.0.1:9081",
		UsersFile:      filepath.Join(dir, "users.json"),
		SessionDir:     filepath.Join(dir, "sessions"),
		SetupTokenFile: filepath.Join(dir, "setup.token"),
		SessionTimeout: time.Hour,
		MaxSessions:    128,
	}
	h, e := admin.New(
		cfg,
		admin.View{
			Role:          c.role,
			AppConfigPath: c.path,
			Stats:         c.stats,
			Validate: func(path string) error {
				_, e := c.load(path)
				return e
			},
			Apply:   c.apply,
			Runtime: c.status,
		},
	)
	if e != nil {
		t.Fatal(e)
	}
	a := &managementAPI{h: h}
	token, _ := os.ReadFile(cfg.SetupTokenFile)
	w := a.call(
		"POST",
		"/api/setup",
		map[string]string{
			"username":    "test-admin",
			"password":    "test-management-password",
			"setup_token": strings.TrimSpace(string(token)),
		},
	)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	a.cookie = w.Result().Cookies()[0]
	a.csrf = apiObject(t, w)["csrf"].(string)
	return a
}
func replaceSetting(s, key, value string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		n, _, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(n) == key {
			lines[i] = key + " = " + value
			return strings.Join(lines, "\n")
		}
	}
	return s + "\n" + key + " = " + value + "\n"
}
func (a *managementAPI) save(t *testing.T, edit func(string) string, want int) {
	t.Helper()
	v := apiObject(t, a.call("GET", "/api/config", nil))
	w := a.call(
		"PUT",
		"/api/config",
		map[string]any{"revision": v["revision"], "content": edit(v["content"].(string)), "apply": true},
	)
	if w.Code != want {
		t.Fatalf("save/apply: %d want %d: %s", w.Code, want, w.Body.String())
	}
}
func waitManaged(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("not released:", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestManagementRepeatedSaveRestartDuplexResources(t *testing.T) {
	for _, tlsMode := range []bool{false, true} {
		t.Run(map[bool]string{false: "WS", true: "WSS"}[tlsMode], func(t *testing.T) {
			baseline := runtime.NumGoroutine()
			var mem runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&mem)
			baseHeap := mem.HeapAlloc
			backend, e := net.Listen("tcp", "127.0.0.1:0")
			if e != nil {
				t.Fatal(e)
			}
			defer backend.Close()
			var active atomic.Int64
			var peers sync.WaitGroup
			acceptDone := make(chan struct{})
			go func() {
				defer close(acceptDone)
				for {
					conn, e := backend.Accept()
					if e != nil {
						return
					}
					active.Add(1)
					peers.Add(1)
					go func() {
						defer peers.Done()
						defer active.Add(-1)
						defer conn.Close()
						io.Copy(conn, conn)
					}()
				}
			}()
			noise := ("heartbeat = 1h\n" +
				"noise-interval-min = 5ms\n" +
				"noise-interval-max = 15ms\n" +
				"noise-min-bytes = 1024\n" +
				"noise-max-bytes = 8192\n")
			serverText := "master_service = " + temporaryAddress(t) + "\ntarget = " + backend.Addr().String() + "\nsession-ttl = 100ms\n" + noise
			ca := ""
			scheme := "ws"
			if tlsMode {
				cert, key := managedCertificate(t)
				serverText += "tls-cert = " + cert + "\ntls-key = " + key + "\n"
				ca = "tls-ca = " + cert + "\n"
				scheme = "wss"
			}
			server := newController(t, "server", serverText)
			client := newController(
				t,
				"client",
				"master_service = "+temporaryAddress(t)+"\nserver = "+scheme+"://"+server.worker.listener.Addr().String()+"/tunnel\n"+ca+noise,
				"-noise-interval-min",
				"0",
				"-noise-interval-max",
				"0",
				"-reconnect-min",
				"1ms",
				"-reconnect-max",
				"5ms",
			)
			sa, caAPI := managementFor(t, server), managementFor(t, client)
			exchange := func(round int) {
				const count = 32
				var wg sync.WaitGroup
				errs := make(chan error, count)
				for n := 0; n < count; n++ {
					wg.Add(1)
					go func(n int) {
						defer wg.Done()
						conn, e := net.DialTimeout("tcp", client.worker.listener.Addr().String(), time.Second)
						if e != nil {
							errs <- e
							return
						}
						defer conn.Close()
						conn.SetDeadline(time.Now().Add(5 * time.Second))
						payload := bytes.Repeat([]byte(fmt.Sprintf("round=%03d stream=%03d|", round, n)), 2048)
						// Half-close and read EOF checks the entire stream, including extra bytes.
						if _, e = conn.Write(payload); e == nil {
							e = conn.(*net.TCPConn).CloseWrite()
						}
						if e != nil {
							errs <- e
							return
						}
						got, e := io.ReadAll(conn)
						if e != nil {
							errs <- e
							return
						}
						if !bytes.Equal(got, payload) {
							errs <- fmt.Errorf("round %d stream %d: got %d want %d", round, n, len(got), len(payload))
						}
					}(n)
				}
				wg.Wait()
				close(errs)
				for e := range errs {
					t.Fatal(e)
				}
				waitManaged(t, "backend connections", func() bool { return active.Load() == 0 })
			}
			backlog := func() func() {
				const count = 32
				conns := make([]net.Conn, 0, count)
				writers := make(chan struct{}, count)
				payload := bytes.Repeat([]byte("pending-data"), 256*1024)
				for n := 0; n < count; n++ {
					conn, e := net.DialTimeout("tcp", client.worker.listener.Addr().String(), time.Second)
					if e != nil {
						t.Fatal(e)
					}
					conns = append(conns, conn)
					t.Cleanup(func() { conn.Close() })
					conn.(*net.TCPConn).SetReadBuffer(4096)
					conn.(*net.TCPConn).SetWriteBuffer(4096)
					conn.SetDeadline(time.Now().Add(10 * time.Second))
					go func() {
						conn.Write(payload)
						writers <- struct{}{}
					}()
				}
				waitManaged(t, "backlogged backend admission", func() bool { return active.Load() > 0 })
				time.Sleep(50 * time.Millisecond)
				return func() {
					// Applying a new proxy intentionally terminates old business
					// streams, including producers blocked by full replay buffers.
					for _, conn := range conns {
						conn.SetReadDeadline(time.Now().Add(time.Second))
						_, e := io.Copy(io.Discard, conn)
						if ne, ok := e.(net.Error); ok && ne.Timeout() {
							t.Fatal("old app socket retained after restart")
						}
						conn.Close()
					}
					for n := 0; n < count; n++ {
						select {
						case <-writers:
						case <-time.After(time.Second):
							t.Fatal("blocked producer retained after restart")
						}
					}
					waitManaged(t, "old backlogged backend sockets", func() bool { return active.Load() == 0 })
				}
			}
			for round := 0; round < 16; round++ {
				exchange(round)
				releaseBacklog := func() {}
				if round%4 == 1 {
					releaseBacklog = backlog()
				}
				oldClient, oldServer := client.worker, server.worker
				priorClient, priorServer := client.stats.Snapshot(), server.stats.Snapshot()
				sa.save(t, func(s string) string { return s + fmt.Sprintf("\n# server generation %d\n", round) }, 200)
				caAPI.save(t, func(s string) string {
					s = replaceSetting(s, "server", scheme+"://"+server.worker.listener.Addr().String()+"/tunnel")
					s = replaceSetting(s, "noise-interval-min", "10ms")
					return s + fmt.Sprintf("\n# client generation %d\n", round)
				}, 200)
				releaseBacklog()
				currentClient, currentServer := client.stats.Snapshot(), server.stats.Snapshot()
				if currentClient.TX < priorClient.TX || currentClient.RX < priorClient.RX ||
					currentServer.TX < priorServer.TX ||
					currentServer.RX < priorServer.RX {
					t.Fatal("proxy restart reset cumulative traffic")
				}

				if client.current.client.Options.NoiseIntervalMin != 0 ||
					client.current.client.Options.NoiseIntervalMax != 0 {
					t.Fatal("explicit CLI zero overridden by saved file")
				}
				for _, old := range []*instance{oldClient, oldServer} {
					select {
					case <-old.done:
					default:
						t.Fatal("previous worker not joined")
					}
				}
				if round%4 == 3 {
					occupied, e := net.Listen("tcp", "127.0.0.1:0")
					if e != nil {
						t.Fatal(e)
					}
					before, _ := os.ReadFile(client.path)
					oldAddress := client.worker.listener.Addr().String()
					generation := client.generation
					caAPI.save(t, func(s string) string { return replaceSetting(s, "master_service", occupied.Addr().String()) }, 409)
					occupied.Close()
					after, _ := os.ReadFile(client.path)
					if !bytes.Equal(before, after) || client.generation != generation ||
						client.worker.listener.Addr().String() != oldAddress {
						t.Fatal("failed apply corrupted config/generation")
					}
					exchange(round + 100)
					runtime.GC()
					runtime.ReadMemStats(&mem)
					t.Logf(
						"round=%d goroutines=%d heap=%d tunnel_connections=%d/%d backend=%d",
						round+1,
						runtime.NumGoroutine(),
						mem.HeapAlloc,
						client.stats.Snapshot().Connections,
						server.stats.Snapshot().Connections,
						active.Load(),
					)
				}
			}
			// Disable remaining padding, wait for counters to quiesce, then independently
			// compare both endpoints' raw TCP counts over the same successful exchange.
			sa.save(t, func(s string) string {
				s = replaceSetting(s, "noise-interval-min", "0")
				return replaceSetting(s, "noise-interval-max", "0")
			}, 200)
			caAPI.save(t, func(s string) string {
				return replaceSetting(s, "server", scheme+"://"+server.worker.listener.Addr().String()+"/tunnel")
			}, 200)
			exchange(200)
			stable := func() {
				previous := client.stats.Snapshot()
				other := server.stats.Snapshot()
				for i := 0; i < 100; i++ {
					time.Sleep(20 * time.Millisecond)
					next, peer := client.stats.Snapshot(), server.stats.Snapshot()
					if next.TX == previous.TX && next.RX == previous.RX && peer.TX == other.TX && peer.RX == other.RX {
						return
					}
					previous, other = next, peer
				}
				t.Fatal("traffic did not quiesce")
			}
			stable()
			cb, sb := client.stats.Snapshot(), server.stats.Snapshot()
			exchange(201)
			stable()
			ce, se := client.stats.Snapshot(), server.stats.Snapshot()
			if ce.TX-cb.TX != se.RX-sb.RX || ce.RX-cb.RX != se.TX-sb.TX {
				t.Fatalf("raw byte mismatch client %d/%d server %d/%d", ce.TX-cb.TX, ce.RX-cb.RX, se.TX-sb.TX, se.RX-sb.RX)
			}
			t.Logf(
				"verified raw TCP byte deltas: client TX/RX=%d/%d server TX/RX=%d/%d",
				ce.TX-cb.TX,
				ce.RX-cb.RX,
				se.TX-sb.TX,
				se.RX-sb.RX,
			)
			if e := client.worker.stop(); e != nil {
				t.Fatal(e)
			}
			if e := server.worker.stop(); e != nil {
				t.Fatal(e)
			}
			waitManaged(t, "all TCP connections", func() bool {
				return client.stats.Snapshot().Connections == 0 && server.stats.Snapshot().Connections == 0 &&
					active.Load() == 0
			})
			backend.Close()
			<-acceptDone
			peers.Wait()
			waitManaged(t, "goroutines", func() bool {
				runtime.GC()
				return runtime.NumGoroutine() <= baseline+4
			})
			runtime.ReadMemStats(&mem)
			t.Logf(
				"released: goroutines=%d baseline=%d heap=%d baseline_heap=%d connections=0",
				runtime.NumGoroutine(),
				baseline,
				mem.HeapAlloc,
				baseHeap,
			)
			if mem.HeapAlloc > baseHeap+24*1024*1024 {
				t.Fatal("retained heap grew beyond 24MiB after cleanup")
			}
		})
	}
}

func managedCertificate(t *testing.T) (string, string) {
	t.Helper()
	seed := httptest.NewTLSServer(http.NotFoundHandler())
	pair := seed.TLS.Certificates[0]
	seed.Close()
	dir := t.TempDir()
	cert, key := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	priv, e := x509.MarshalPKCS8PrivateKey(pair.PrivateKey)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pair.Certificate[0]}), 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: priv}), 0600); e != nil {
		t.Fatal(e)
	}
	return cert, key
}
