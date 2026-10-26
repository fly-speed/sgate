package tunnel

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestConnectivityProbeWSWSS(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprint(secure), func(t *testing.T) {
			backend, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer backend.Close()
			var active, accepted, payload atomic.Int64
			go func() {
				for {
					conn, err := backend.Accept()
					if err != nil {
						return
					}
					active.Add(1)
					accepted.Add(1)
					go func() { defer active.Add(-1); defer conn.Close(); n, _ := io.Copy(io.Discard, conn); payload.Add(n) }()
				}
			}()
			closed, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			bad := closed.Addr().String()
			closed.Close()
			server, err := NewServer(ServerConfig{Key: bytes.Repeat([]byte{65}, 32), Password: testPassword,
				Target: backend.Addr().String(), AllowedTargets: []string{bad}, Options: testOptions()})
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			var web *httptest.Server
			if secure {
				web = httptest.NewTLSServer(server)
			} else {
				web = httptest.NewServer(server)
			}
			defer web.Close()
			config := ClientConfig{Key: server.cfg.Key, Password: testPassword,
				URL: "ws" + strings.TrimPrefix(web.URL, "http") + "/tunnel", Options: testOptions()}
			if secure {
				roots := x509.NewCertPool()
				roots.AddCert(web.Certificate())
				config.TLSConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
			}
			probe := func(cfg ClientConfig) ProbeResult {
				t.Helper()
				client, err := NewClient(cfg)
				if err != nil {
					t.Fatal(err)
				}
				return client.Probe(context.Background())
			}
			for i := 0; i < 10; i++ {
				if result := probe(config); !result.OK || result.Stage != "backend" {
					t.Fatalf("successful probe: %+v", result)
				}
			}
			config.Addr = bad
			if result := probe(config); result.OK || result.Stage != "backend" || !strings.Contains(result.Detail, "refused") {
				t.Fatalf("refused: %+v", result)
			}
			config.OmitAddr = true
			if result := probe(config); !result.OK {
				t.Fatalf("send-addr=false did not use default: %+v", result)
			}
			config.OmitAddr, config.Addr = false, "127.0.0.1:1"
			if result := probe(config); result.OK || !strings.Contains(result.Detail, "not allowed") {
				t.Fatalf("allowlist: %+v", result)
			}
			config.Addr, config.Password = "", "wrong-probe-password"
			if result := probe(config); result.OK || !strings.Contains(result.Detail, "authentication rejected") {
				t.Fatalf("auth: %+v", result)
			}
			config.Password = testPassword
			if secure {
				config.TLSConfig = nil
				if result := probe(config); result.OK {
					t.Fatalf("untrusted TLS: %+v", result)
				}
			}
			waitFor(t, "probe TCP connections released", func() bool { return accepted.Load() == 11 && active.Load() == 0 })
			if payload.Load() != 0 {
				t.Fatal("probe sent application data")
			}
			server.mu.Lock()
			if len(server.sessions) != 0 || len(server.probeGate) != 0 {
				t.Error("probe retained tunnel sessions/work")
			}
			server.mu.Unlock()
		})
	}
}

func TestConnectivityProbeLegacyAndProof(t *testing.T) {
	key := bytes.Repeat([]byte{66}, 32)
	for _, kind := range []string{"legacy", "forged-failure", "forged-success"} {
		t.Run(kind, func(t *testing.T) {
			web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch kind {
				case "legacy":
					if r.Header.Get("X-Sgate-Resume") != "1" {
						t.Error("legacy probe would create retained session")
					}
					http.Error(w, "session expired", 410)
				case "forged-failure":
					w.Header().Set("X-Sgate-Probe", "4")
					http.Error(w, "error", 502)
				default:
					request := tunnelRequest{id: r.Header.Get("X-Sgate-Session"), nonce: r.Header.Get("X-Sgate-Nonce"), timestamp: r.Header.Get("X-Sgate-Time"), credential: r.Header.Get(passwordHeader)}
					headers := http.Header{}
					nonce := randomID()
					headers.Set("X-Sgate-Nonce", nonce)
					headers.Set("X-Sgate-Auth", mac(key, protocol, "server", request.id, request.nonce, nonce, request.timestamp, request.credential))
					ws, err := (&websocket.Upgrader{}).Upgrade(w, r, headers)
					if err == nil {
						ws.Close()
					}
				}
			}))
			defer web.Close()
			client, err := NewClient(ClientConfig{Key: key, Password: testPassword, URL: "ws" + strings.TrimPrefix(web.URL, "http") + "/tunnel", Options: testOptions()})
			if err != nil {
				t.Fatal(err)
			}
			result := client.Probe(context.Background())
			if result.OK {
				t.Fatalf("unverified result accepted: %+v", result)
			}
			if kind == "legacy" && !strings.Contains(result.Detail, "upgrade the server") {
				t.Fatal(result)
			}
		})
	}
}

func TestConnectivityProbeCancellation(t *testing.T) {
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer web.Close()
	client, err := NewClient(ClientConfig{Key: bytes.Repeat([]byte{67}, 32), Password: testPassword, URL: "ws" + strings.TrimPrefix(web.URL, "http") + "/tunnel", Options: testOptions()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	result := client.Probe(ctx)
	if result.OK || time.Since(started) > time.Second {
		t.Fatal("probe ignored cancellation", result)
	}
}
