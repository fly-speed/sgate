package app

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"sgate/internal/vpn"
)

type vpnTestDevice struct {
	done chan struct{}
	once sync.Once
}

func (d *vpnTestDevice) Name() string                { return "fake-tun" }
func (d *vpnTestDevice) Read([]byte) (int, error)    { <-d.done; return 0, io.EOF }
func (d *vpnTestDevice) Write(p []byte) (int, error) { return len(p), nil }
func (d *vpnTestDevice) Close() error                { d.once.Do(func() { close(d.done) }); return nil }
func TestVPNOnlyBuildDoesNotRequireProxyServerOrPassword(t *testing.T) {
	c := newController(t, "client", "server = ws://127.0.0.1:1/tunnel\n")
	fs := appFlags("client")
	fs.Set("key-file", c.original.Lookup("key-file").Value.String())
	fs.Set("vpn-mode", "only")
	fs.Set("vpn-server", "quic://vpn.invalid:8444/vpn")
	d, e := build("client", fs, c.stats)
	if e != nil {
		t.Fatal(e)
	}
	if !d.vpnOnly || d.client == nil || d.vpn == nil || len(d.proxies) != 0 {
		t.Fatal("VPN-only still depends on proxy")
	}
	// Only build proxy admission: no TUN is opened by this test.
	d.vpn = nil
	i, e := d.start(c.ctx, c.stats)
	if e != nil {
		t.Fatal(e)
	}
	defer i.stop()
	conn, e := net.Dial("tcp", i.listener.Addr().String())
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(time.Second))
	_, e = conn.Read(make([]byte, 1))
	if e != io.EOF {
		t.Fatal("VPN-only must reject business TCP", e)
	}
}
func TestVPNWorkerFailureKeepsManagementRuntimeAvailable(t *testing.T) {
	p := &vpn.Prepared{Role: "client", Mode: "only", Host: "127.0.0.1", Port: "8444", Config: vpn.Config{Network: netip.MustParsePrefix("10.77.0.0/24")}}
	i := &instance{}
	i.startVPNWithDevice(context.Background(), p, func(string) (vpnDevice, error) { return nil, errors.New("test permission denied") })
	<-i.vpn.done
	h := i.vpn.snapshot()
	if h.State != "failed" || h.Healthy || h.LastError == "" {
		t.Fatal(h)
	}
	i.vpn.stop()
	i.vpn.stop()
}
func TestVPNWorkerServerLifecycleUsesInMemoryTUN(t *testing.T) {
	tlsServer := httptest.NewTLSServer(nil)
	tlsConfig := tlsServer.TLS.Clone()
	tlsServer.Close()
	p := &vpn.Prepared{Role: "server", Mode: "only", Config: vpn.Config{Address: "127.0.0.1:0", TLS: tlsConfig, Key: make([]byte, 32), Network: netip.MustParsePrefix("10.77.0.0/24"), MTU: 1280}, Network: vpn.NetworkOptions{Server: true}}
	dev := &vpnTestDevice{done: make(chan struct{})}
	i := &instance{}
	i.startVPNWithDevice(context.Background(), p, func(string) (vpnDevice, error) { return dev, nil })
	deadline := time.Now().Add(3 * time.Second)
	for {
		h := i.vpn.snapshot()
		if h.State == "listening" {
			if !h.Healthy || h.Device != "fake-tun" {
				t.Fatal(h)
			}
			break
		}
		if h.State == "failed" || time.Now().After(deadline) {
			t.Fatal(h)
		}
		time.Sleep(time.Millisecond)
	}
	i.vpn.stop()
	if h := i.vpn.snapshot(); h.State != "stopped" || h.Healthy {
		t.Fatal(h)
	}
	select {
	case <-dev.done:
	default:
		t.Fatal("TUN leaked")
	}
}

func TestVPNSharedApplyPreservesUnchangedWorkerAndStopsDisabled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	workerCtx, stop := context.WithCancel(ctx)
	go func() { <-workerCtx.Done(); close(done) }()
	p := &vpn.Prepared{Fingerprint: "same"}
	w := &vpnWorker{stats: &vpn.Stats{}, health: vpnHealth{Enabled: true, Healthy: true, State: "listening"}, cancel: stop, done: done}
	c := &controller{ctx: ctx, current: definition{vpn: p}, worker: &instance{vpn: w}}
	c.replaceVPN(definition{vpn: &vpn.Prepared{Fingerprint: "same"}})
	if c.worker.vpn != w {
		t.Fatal("unchanged VPN restarted")
	}
	c.replaceVPN(definition{})
	if c.worker.vpn != nil {
		t.Fatal("disabled VPN retained")
	}
	select {
	case <-done:
	default:
		t.Fatal("VPN not joined before disabling")
	}
}

func TestVPNOnlySharedHTTPRejectsProxyAndNativeQUIC(t *testing.T) {
	s := &sharedServer{}
	s.vpnOnly.Store(true)
	response := httptest.NewRecorder()
	s.ServeHTTP(response, httptest.NewRequest("GET", "/tunnel", nil))
	if response.Code != 404 || s.NativeQUICServer() != nil {
		t.Fatal("VPN-only admitted proxy", response.Code)
	}
}

func TestVPNServerBuildSupportsMacOSAndLinux(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("unsupported VPN platform")
	}
	key := filepath.Join(t.TempDir(), "vpn.key")
	if e := os.WriteFile(key, []byte(strings.Repeat("01", 32)), 0600); e != nil {
		t.Fatal(e)
	}
	cert, private := managedCertificate(t)
	fs := appFlags("server")
	fs.Set("key-file", key)
	fs.Set("vpn-mode", "only")
	fs.Set("vpn-tls-cert", cert)
	fs.Set("vpn-tls-key", private)
	d, e := build("server", fs, nil)
	if e != nil {
		t.Fatal(e)
	}
	if !d.vpnOnly || d.vpn == nil || d.vpn.Role != "server" || !d.vpn.Network.Server || !vpn.PlatformSupport("server").Supported {
		t.Fatal("server not enabled on supported platform")
	}
}
