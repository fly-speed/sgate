package app

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	master "github.com/acl-dev/go-service"
	"sgate/internal/admin"
	"sgate/internal/tunnel"
)

type onlineTCP struct {
	*master.TcpService
	managed bool
}

func (s onlineTCP) Configure(a string) (net.Listener, error) { return s.TcpService.Configure(a) }
func (s onlineTCP) ActiveConnections() int                   { return master.ConnCountCur() }
func (s onlineTCP) Managed() bool                            { return s.managed }
func onlineAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := l.Addr().String()
	l.Close()
	return a
}
func onlineBackend(t *testing.T, marker string) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var peers sync.Map
	t.Cleanup(func() { l.Close(); peers.Range(func(k, v any) bool { k.(net.Conn).Close(); return true }) })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			peers.Store(c, true)
			go func() { defer c.Close(); defer peers.Delete(c); c.Write([]byte(marker)); io.Copy(c, c) }()
		}
	}()
	return l.Addr().String()
}
func setOnlineMappings(t *testing.T, c *controller, routes []clientProxy) {
	t.Helper()
	if len(routes) == 0 {
		setConfig(t, c, "proxies", "''")
		return
	}
	b, _ := json.Marshal(routes)
	setConfig(t, c, "proxies", "'"+string(b)+"'")
}
func startOnlineController(t *testing.T, text string, managed bool, extra ...string) *controller {
	t.Helper()
	c := proxyConfigController(t, text, extra...)
	d, err := c.load("")
	if err != nil {
		t.Fatal(err)
	}
	service, err := master.TcpServiceInit(d.listen, master.WithTCPContext(context.Background()), master.WithTCPManaged(false))
	if err != nil {
		t.Fatal(err)
	}
	service.AcceptHandler = service.AcceptConn
	c.tcp = onlineTCP{service, managed}
	c.ctx = service.Context()
	c.controls = map[string]string{}
	c.servicePath = c.path
	d.tcp = c.tcp
	c.current = d
	c.worker, err = d.start(c.ctx, c.stats)
	if err != nil {
		service.Close()
		t.Fatal(err)
	}
	c.generation = 1
	done := make(chan struct{})
	go func() { defer close(done); service.Run() }()
	t.Cleanup(func() { c.worker.stop(); service.Close(); <-done })
	return c
}
func onlineConnect(t *testing.T, address, marker string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(5 * time.Second))
	b := make([]byte, len(marker))
	if _, err = io.ReadFull(c, b); err != nil || string(b) != marker {
		t.Fatalf("backend marker %q %v", b, err)
	}
	return c
}
func onlineExchange(t *testing.T, c net.Conn) {
	t.Helper()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	p := bytes.Repeat([]byte("duplex-record"), 3000)
	c.Write(p)
	b := make([]byte, len(p))
	if _, err := io.ReadFull(c, b); err != nil || !bytes.Equal(b, p) {
		t.Fatal("duplex data changed", err)
	}
}

func TestOnlineMappingsKeepUnchangedConnectionsAndRollback(t *testing.T) {
	for _, tlsMode := range []bool{false, true} {
		t.Run(fmt.Sprint(tlsMode), func(t *testing.T) {
			a, b := onlineBackend(t, "A"), onlineBackend(t, "B")
			server, err := tunnel.NewServer(tunnel.ServerConfig{Key: bytes.Repeat([]byte{0x39}, 32), Password: "multi-test-password", Target: a, AllowedTargets: []string{a, b}, SessionTTL: 100 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			var web *httptest.Server
			if tlsMode {
				web = httptest.NewTLSServer(server)
			} else {
				web = httptest.NewServer(server)
			}
			defer web.Close()
			endpoint := "ws" + strings.TrimPrefix(web.URL, "http") + "/tunnel"
			first, second, fallback := onlineAddress(t), onlineAddress(t), onlineAddress(t)
			text := "master_service = " + fallback + "\nserver = " + endpoint + "\nnoise-interval-min = 1ms\nnoise-interval-max = 3ms\n"
			if tlsMode {
				ca := filepath.Join(t.TempDir(), "ca.pem")
				os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: web.Certificate().Raw}), 0600)
				text += "tls-ca = " + ca + "\n"
			}
			raw, _ := json.Marshal([]clientProxy{{Listen: first, Server: endpoint + "?addr=" + a}})
			text += "proxies = '" + string(raw) + "'\n"
			c := startOnlineController(t, text, false)
			keep := onlineConnect(t, first, "A")
			onlineExchange(t, keep)
			worker := c.worker
			routes := []clientProxy{{Listen: first, Server: endpoint, Addr: a}, {Listen: second, Server: endpoint, Addr: b}}
			setOnlineMappings(t, c, routes)
			if err := c.applyLive([]string{"proxies"}); err != nil {
				t.Fatal(err)
			}
			onlineExchange(t, keep)
			added := onlineConnect(t, second, "B")
			onlineExchange(t, added)
			if c.worker != worker || c.generation != 1 {
				t.Fatal("online edit restarted whole proxy")
			}
			occupied, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer occupied.Close()
			setOnlineMappings(t, c, append(routes, clientProxy{Listen: occupied.Addr().String(), Server: endpoint}))
			if c.applyLive([]string{"proxies"}) == nil {
				t.Fatal("occupied addition applied")
			}
			onlineExchange(t, keep)
			onlineExchange(t, added)
			for i := 0; i < 5; i++ {
				setOnlineMappings(t, c, routes[:1])
				if err := c.applyLive([]string{"proxies"}); err != nil {
					t.Fatal(err)
				}
				onlineExchange(t, keep)
				probe, err := net.Listen("tcp", second)
				if err != nil {
					t.Fatal("deleted listener leaked", err)
				}
				probe.Close()
				setOnlineMappings(t, c, routes)
				if err := c.applyLive([]string{"proxies"}); err != nil {
					t.Fatal(err)
				}
				onlineExchange(t, keep)
			}
			setOnlineMappings(t, c, nil)
			if err := c.applyLive([]string{"proxies"}); err != nil {
				t.Fatal(err)
			}
			single := onlineConnect(t, fallback, "A")
			onlineExchange(t, single)
			setOnlineMappings(t, c, routes)
			if err := c.applyLive([]string{"proxies"}); err != nil {
				t.Fatal(err)
			}
			onlineExchange(t, onlineConnect(t, second, "B"))
		})
	}
}

func TestManagedOnlineMappingPortChangeRequestsRestart(t *testing.T) {
	first, second := onlineAddress(t), onlineAddress(t)
	raw, _ := json.Marshal([]clientProxy{{Listen: first, Server: "ws://127.0.0.1:1/tunnel"}})
	c := startOnlineController(t, "proxies = '"+string(raw)+"'\n", true)
	old := c.worker
	setOnlineMappings(t, c, []clientProxy{{Listen: first, Server: "ws://127.0.0.1:1/tunnel"}, {Listen: second, Server: "ws://127.0.0.1:1/tunnel"}})
	if err := c.applyLive([]string{"proxies"}); !errors.Is(err, admin.ErrMappingServiceRestart) {
		t.Fatal(err)
	}
	if c.worker != old || len(c.current.proxies) != 1 {
		t.Fatal("managed change touched current listeners")
	}
	want := first + "," + second
	if got := strings.Join(c.status().(map[string]any)["pending_listeners"].([]string), ","); got != want {
		t.Fatalf("pending listeners = %q, want %q", got, want)
	}
	setOnlineMappings(t, c, []clientProxy{{Listen: first, Server: "ws://127.0.0.1:1/tunnel"}})
	if err := c.applyLive([]string{"proxies"}); err != nil {
		t.Fatal(err)
	}
	if c.pendingListeners != "" {
		t.Fatal("restoring current ports retained stale pending listeners")
	}
}
