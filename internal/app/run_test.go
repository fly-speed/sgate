package app

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"flag"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sgate/internal/cli"
	"sgate/internal/tunnel"
	"sgate/internal/vpn"
)

func appFlags(role string) *flag.FlagSet {
	fs := flag.NewFlagSet(role, flag.ContinueOnError)
	vpn.RegisterFlags(fs, role)
	for _, v := range []struct{ n, v string }{
		{"key-file", ""},
		{"password", ""},
		{
			"admin-config",
			"",
		},
	} {
		fs.String(v.n, v.v, "")
	}
	fs.Int("max-streams", 128, "")
	cli.ResourceFlags(fs)
	fs.Int("buffer-bytes", 4194304, "")
	fs.Duration("heartbeat", 20*time.Second, "")
	fs.Duration("heartbeat-timeout", 0, "")
	cli.NoisePolicyFlags(fs)
	fs.Duration("noise-interval", 0, "")
	fs.Duration("noise-interval-min", 0, "")
	fs.Duration("noise-interval-max", 0, "")
	cli.ByteSizeVar(fs, new(int), "noise-min-bytes", 1024, "")
	cli.ByteSizeVar(fs, new(int), "noise-max-bytes", 10240, "")
	fs.String("master_service", "127.0.0.1|0", "")
	if role == "client" {
		fs.String("proxies", "", "")
		fs.String("proxies-file", "", "")
		fs.String("username", "", "")
		fs.String("quic-mode", "native", "")
		fs.String("addr", "", "")
		fs.Bool("send-addr", true, "")
		fs.Bool("allow-reverse", false, "")
		fs.String("tls-ca", "", "")
		fs.Duration("reconnect-min", 250*time.Millisecond, "")
		fs.Duration("reconnect-max", 10*time.Second, "")
		fs.Duration("offline-timeout", 2*time.Minute, "")
	} else {
		fs.String("target", "", "")
		fs.String("accounts-file", "", "")
		fs.String("allowed-targets", "", "")
		fs.String("path", "/tunnel", "")
		fs.String("quic-listen", "", "")
		fs.String("tls-cert", "", "")
		fs.String("tls-key", "", "")
		fs.Duration("session-ttl", 2*time.Minute, "")
		fs.Int("max-sessions", 128, "")
	}
	return fs
}
func newController(t *testing.T, role, content string, extra ...string) *controller {
	t.Helper()
	dir := t.TempDir()
	key := filepath.Join(dir, "key")
	os.WriteFile(key, []byte(hex.EncodeToString(bytes.Repeat([]byte{39}, 32))), 0600)
	path := filepath.Join(dir, "app.conf")
	content = "key-file = " + key + "\npassword = app-test-password\n" + content
	if e := os.WriteFile(path, []byte(content), 0600); e != nil {
		t.Fatal(e)
	}
	fs := appFlags(role)
	args := append([]string{"-config", path}, extra...)
	if e := cli.Parse(fs, args); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &controller{ctx: ctx, original: fs, args: args, role: role, path: path, stats: tunnel.NewStats()}
	t.Cleanup(func() {
		cancel()
		if c.worker != nil {
			if e := c.worker.stop(); e != nil {
				t.Error(e)
			}
		}
	})
	d, e := c.load("")
	if e != nil {
		t.Fatal(e)
	}
	c.current = d
	c.worker, e = d.start(ctx, c.stats)
	if e != nil {
		t.Fatal(e)
	}
	c.generation = 1
	return c
}
func setConfig(t *testing.T, c *controller, key, value string) {
	t.Helper()
	b, e := os.ReadFile(c.path)
	if e != nil {
		t.Fatal(e)
	}
	lines := strings.Split(string(b), "\n")
	found := false
	for i, line := range lines {
		n, _, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(n) == key {
			lines[i] = key + " = " + value
			found = true
		}
	}
	if !found {
		lines = append(lines, key+" = "+value)
	}
	if e = os.WriteFile(c.path, []byte(strings.Join(lines, "\n")), 0600); e != nil {
		t.Fatal(e)
	}
}
func TestApplyRestartAndBindRollback(t *testing.T) {
	for _, role := range []string{"client", "server"} {
		t.Run(role, func(t *testing.T) {
			content := "target = 127.0.0.1:1\n"
			if role == "client" {
				content = testClientMapping(t, "ws://127.0.0.1:1/tunnel")
			}
			c := newController(t, role, content)
			first := c.worker
			setConfig(t, c, "noise-interval-min", "1s")
			setConfig(t, c, "noise-interval-max", "3s")
			if e := c.apply(); e != nil {
				t.Fatal(e)
			}
			if c.worker == first || c.generation != 2 {
				t.Fatal("proxy not replaced")
			}
			select {
			case <-first.done:
			default:
				t.Fatal("old worker not joined")
			}
			occupied, e := net.Listen("tcp", "127.0.0.1:0")
			if e != nil {
				t.Fatal(e)
			}
			defer occupied.Close()
			listenKey := "master_service"
			if role == "client" {
				setConfig(t, c, "proxies", testClientMappingJSON(t, occupied.Addr().String(), "ws://127.0.0.1:1/tunnel"))
			} else {
				setConfig(t, c, listenKey, occupied.Addr().String())
			}
			if e = c.apply(); e == nil || !strings.Contains(e.Error(), "已恢复") {
				t.Fatal("missing rollback", e)
			}
			if c.worker == nil || c.generation != 2 {
				t.Fatal("old proxy not restored")
			}
			probe, e := net.DialTimeout("tcp", c.worker.listener.Addr().String(), time.Second)
			if e != nil {
				t.Fatal("restored listener unavailable", e)
			}
			probe.Close()
		})
	}
}
func TestApplyKeepsCLIOverrideAndRejectsBadCandidate(t *testing.T) {
	c := newController(
		t,
		"client",
		(testClientMapping(t, "ws://127.0.0.1:1/tunnel") + "" +
			"noise-interval-min = 1s\n" +
			"noise-interval-max = 3s\n"),
		"-noise-interval-min",
		"0",
		"-noise-interval-max",
		"0",
	)
	setConfig(t, c, "noise-interval-min", "2s")
	if e := c.apply(); e != nil {
		t.Fatal(e)
	}
	if c.current.client.Options.NoiseIntervalMin != 0 || c.current.client.Options.NoiseIntervalMax != 0 {
		t.Fatal("CLI override lost on reload")
	}
	setConfig(t, c, "heartbeat", "invalid")
	if _, e := c.load(c.path); e == nil {
		t.Fatal("invalid config accepted")
	}
	if c.worker == nil {
		t.Fatal("validation stopped running proxy")
	}
}
func TestManagedClientServerDuplexAndStats(t *testing.T) {
	for _, tlsMode := range []bool{false, true} {
		t.Run(map[bool]string{false: "WS", true: "WSS"}[tlsMode], func(t *testing.T) {
			backend, e := net.Listen("tcp", "127.0.0.1:0")
			if e != nil {
				t.Fatal(e)
			}
			defer backend.Close()
			go func() {
				for {
					conn, e := backend.Accept()
					if e != nil {
						return
					}
					go func() {
						defer conn.Close()
						io.Copy(conn, conn)
					}()
				}
			}()
			serverText := "target = " + backend.Addr().String() + "\n"
			ca := ""
			if tlsMode {
				seed := httptest.NewTLSServer(http.NotFoundHandler())
				pair := seed.TLS.Certificates[0]
				seed.Close()
				dir := t.TempDir()
				certPath, keyPath := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
				priv, e := x509.MarshalPKCS8PrivateKey(pair.PrivateKey)
				if e != nil {
					t.Fatal(e)
				}
				os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pair.Certificate[0]}), 0600)
				os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: priv}), 0600)
				serverText += "tls-cert = " + certPath + "\ntls-key = " + keyPath + "\n"
				ca = "tls-ca = " + certPath + "\n"
			}
			server := newController(t, "server", serverText)
			scheme := "ws"
			if tlsMode {
				scheme = "wss"
			}
			client := newController(t, "client", testClientMapping(t, scheme+"://"+server.worker.listener.Addr().String()+"/tunnel")+ca)
			exchange := func() {
				conn, e := net.DialTimeout("tcp", client.worker.listener.Addr().String(), time.Second)
				if e != nil {
					t.Fatal(e)
				}
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(5 * time.Second))
				payload := bytes.Repeat([]byte("managed-duplex"), 2000)
				if _, e = conn.Write(payload); e != nil {
					t.Fatal(e)
				}
				got := make([]byte, len(payload))
				if _, e = io.ReadFull(conn, got); e != nil {
					t.Fatal(e)
				}
				if !bytes.Equal(got, payload) {
					t.Fatal("corrupted payload")
				}
			}
			exchange()
			if e := client.apply(); e != nil {
				t.Fatal(e)
			}
			exchange()
			// Server restart invalidates retained old sessions; use its new bound port.
			if e := server.apply(); e != nil {
				t.Fatal(e)
			}
			setConfig(t, client, "proxies", strings.TrimSuffix(strings.TrimPrefix(testClientMappingAt(t, client.current.listen, scheme+"://"+server.worker.listener.Addr().String()+"/tunnel"), "proxies = '"), "'\n"))
			if e := client.apply(); e != nil {
				t.Fatal(e)
			}
			exchange()
			for _, s := range []*tunnel.Stats{client.stats, server.stats} {
				v := s.Snapshot()
				if v.TX == 0 || v.RX == 0 || v.Connections < 1 {
					t.Fatal("missing real tunnel traffic", v)
				}
			}
		})
	}
}

// Build current mapping-only fixtures. Ports are reserved per test rather than
// relying on an implicit endpoint inherited from master_service/server.
func testClientMapping(t *testing.T, endpoint string) string {
	t.Helper()
	return testClientMappingAt(t, "", endpoint)
}
func testClientMappingAt(t *testing.T, listen, endpoint string, reverse ...bool) string {
	t.Helper()
	if listen == "" {
		listen = temporaryAddress(t)
	}
	route := clientProxy{Listen: listen, Server: endpoint}
	if len(reverse) > 0 {
		route.AllowReverse = reverse[0]
	}
	raw, err := json.Marshal([]clientProxy{route})
	if err != nil {
		t.Fatal(err)
	}
	return "proxies = '" + string(raw) + "'\n"
}

func testClientMappingJSON(t *testing.T, listen, endpoint string) string {
	t.Helper()
	return strings.TrimSuffix(strings.TrimPrefix(testClientMappingAt(t, listen, endpoint), "proxies = '"), "'\n")
}
