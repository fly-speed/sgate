package app

import (
	"bytes"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"sgate/internal/admin"
	"sgate/internal/tunnel"
)

func TestProxyFileIndependentCredentialsLiveDuplexReconnect(t *testing.T) {
	for _, secure := range []bool{false, true} {
		for _, managed := range []bool{false, true} {
			t.Run(fmt.Sprintf("wss=%t/managed=%t", secure, managed), func(t *testing.T) {
				a, b := onlineBackend(t, "A"), onlineBackend(t, "B")
				serverStats := tunnel.NewStats()
				server, err := tunnel.NewServer(tunnel.ServerConfig{Key: bytes.Repeat([]byte{0x39}, 32), Target: a, AllowedTargets: []string{a, b}, SessionTTL: time.Second,
					Accounts: []tunnel.Account{{Username: "alice", Password: "alice-secret"}, {Username: "bob", Password: "bob-secret"}, {Username: "carol", Password: "carol-secret"}},
					Options:  tunnel.Options{Stats: serverStats, NoiseIntervalMin: time.Millisecond, NoiseIntervalMax: 3 * time.Millisecond}})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(server.Close)
				var connections sync.Map
				web := httptest.NewUnstartedServer(server)
				web.Config.ConnState = func(c net.Conn, state http.ConnState) {
					if state == http.StateHijacked {
						connections.Store(c, true)
					}
				}
				if secure {
					web.StartTLS()
				} else {
					web.Start()
				}
				t.Cleanup(func() { connections.Range(func(k, v any) bool { k.(net.Conn).Close(); return true }); web.Close() })
				endpoint := "ws" + strings.TrimPrefix(web.URL, "http") + "/tunnel"
				first, second := onlineAddress(t), onlineAddress(t)
				routes := []clientProxy{{Listen: first, Server: endpoint, Addr: a, Username: credential("alice"), Password: credential("alice-secret")}, {Listen: second, Server: endpoint, Addr: b, Username: credential("bob"), Password: credential("bob-secret")}}
				raw, _ := json.Marshal(routes)
				text := "proxies = '" + string(raw) + "'\nnoise-interval-min = 1ms\nnoise-interval-max = 3ms\n"
				if secure {
					ca := filepath.Join(t.TempDir(), "ca.pem")
					if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: web.Certificate().Raw}), 0600); err != nil {
						t.Fatal(err)
					}
					text += "tls-ca = " + ca + "\n"
				}
				c := startOnlineController(t, text, managed)
				one, two := onlineConnect(t, first, "A"), onlineConnect(t, second, "B")
				onlineExchange(t, one)
				onlineExchange(t, two)
				file := filepath.Join(filepath.Dir(c.path), "routes.json")
				if err := os.WriteFile(file, raw, 0600); err != nil {
					t.Fatal(err)
				}
				setConfig(t, c, "proxies-file", "routes.json")
				if err := c.applyLive([]string{"proxies-file"}); err != nil {
					t.Fatal(err)
				}
				onlineExchange(t, one)
				onlineExchange(t, two)
				group := c.worker.proxies
				group.mu.RLock()
				oldFirst, oldSecond := group.routes[c.current.proxies[0].port], group.routes[c.current.proxies[1].port]
				group.mu.RUnlock()
				// Every forced disconnect must resume the same per-mapping account/session.
				for i := 0; i < 3; i++ {
					connections.Range(func(k, v any) bool { k.(net.Conn).Close(); connections.Delete(k); return true })
					onlineExchange(t, one)
					onlineExchange(t, two)
				}
				routes[0].Username, routes[0].Password = credential("carol"), credential("carol-secret")
				raw, _ = json.Marshal(routes)
				if err := os.WriteFile(file, raw, 0600); err != nil {
					t.Fatal(err)
				}
				if err := c.applyLive([]string{"proxies"}); err != nil {
					t.Fatal(err)
				}
				group.mu.RLock()
				newFirst, newSecond := group.routes[c.current.proxies[0].port], group.routes[c.current.proxies[1].port]
				group.mu.RUnlock()
				if oldFirst == newFirst || oldSecond != newSecond || c.generation != 1 {
					t.Fatal("credential edit did not isolate mapping replacement")
				}
				select {
				case <-oldFirst.done:
				default:
					t.Fatal("old authenticated worker not released")
				}
				onlineExchange(t, two)
				replacement := onlineConnect(t, first, "A")
				onlineExchange(t, replacement)
				// Probe must use its own credentials rather than the first mapping's identity.
				result, err := c.testProxy(c.ctx, admin.ProxyMapping{Listen: second, Server: endpoint, Addr: b, Username: credential("bob"), Password: credential("bob-secret")})
				if err != nil || !result.OK {
					t.Fatal("independent mapping probe failed", result, err)
				}
				one.Close()
				two.Close()
				replacement.Close()
				if err := c.worker.stop(); err != nil {
					t.Fatal(err)
				}
				server.Close()
				if stats := c.stats.Snapshot(); stats.Connections != 0 || stats.NoiseActivity.Tunnels != 0 || stats.NoiseActivity.Streams != 0 {
					t.Fatal("client resources retained", stats)
				}
				if stats := serverStats.Snapshot(); stats.NoiseActivity.Tunnels != 0 || stats.NoiseActivity.Streams != 0 {
					t.Fatal("server sessions retained", stats)
				}
			})
		}
	}
}
