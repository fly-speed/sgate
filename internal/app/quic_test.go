package app

import (
	"context"
	mastergin "github.com/acl-dev/gin-service"
	"github.com/gin-gonic/gin"
	"io"
	"net"
	"testing"
	"time"

	"sgate/internal/tunnel"
)

func TestQUICWorkerConfigurationAndShutdown(t *testing.T) {
	cert, key := managedCertificate(t)
	backend, _ := probeBackend(t)
	c := newController(t, "server", "target = "+backend+"\nquic-listen = 127.0.0.1:0\ntls-cert = "+cert+"\ntls-key = "+key+"\n")
	address := c.worker.quicListener.Addr().String()
	trust, err := clientTLSConfig(cert)
	if err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"quic://" + address + "/tunnel", "wss://" + c.worker.listener.Addr().String() + "/tunnel"} {
		client, err := tunnel.NewClient(tunnel.ClientConfig{Key: c.current.server.Key, Password: "app-test-password", URL: endpoint, TLSConfig: trust})
		if err != nil {
			t.Fatal(err)
		}
		if result := client.Probe(context.Background()); !result.OK {
			t.Fatalf("%s probe failed: %+v", endpoint, result)
		}
	}
	if err := c.worker.stop(); err != nil {
		t.Fatal(err)
	}
	c.worker = nil
	// Worker shutdown must release the UDP socket immediately.
	packet, err := net.ListenPacket("udp", address)
	if err != nil {
		t.Fatal("UDP listener leaked:", err)
	}
	packet.Close()
	// Validate config before attempting to replace the running worker.
	setConfig(t, c, "tls-cert", "")
	setConfig(t, c, "tls-key", "")
	if _, err := c.load(""); err == nil {
		t.Fatal("QUIC without a certificate accepted")
	}
}

func TestQUICBindFailureReleasesTCPListener(t *testing.T) {
	cert, key := managedCertificate(t)
	c := newController(t, "server", "target = localhost:22\ntls-cert = "+cert+"\ntls-key = "+key+"\n")
	blocker, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	next := c.current
	next.listen = onlineAddress(t)
	next.quicListen = blocker.LocalAddr().String()
	if worker, err := next.start(context.Background(), tunnel.NewStats()); err == nil {
		worker.stop()
		t.Fatal("UDP bind collision accepted")
	}
	listener, err := net.Listen("tcp", next.listen)
	if err != nil {
		t.Fatal("failed start leaked TCP listener", err)
	}
	listener.Close()
}

func TestQUICFrameworkSharedProxyReplacement(t *testing.T) {
	cert, key := managedCertificate(t)
	backend, _ := probeBackend(t)
	replacement, _ := probeBackend(t)
	c := newController(t, "server", "master_service = "+onlineAddress(t)+"\ntarget = "+backend+"\nquic-listen = 127.0.0.1:0\ntls-cert = "+cert+"\ntls-key = "+key+"\n")
	address := c.worker.listener.Addr().String()
	if err := c.worker.stop(); err != nil {
		t.Fatal(err)
	}
	c.worker = nil
	framework, err := mastergin.Init(address, mastergin.WithContext(c.ctx), mastergin.WithManaged(false))
	if err != nil {
		t.Fatal(err)
	}
	framework.GET("/*path", gin.WrapH(framework.Handler()))
	service := reverseLeaseHTTP{framework}
	d := c.current
	d.service, d.shared = service, &sharedServer{}
	c.service, c.shared, c.current = service, d.shared, d
	c.worker, err = d.start(c.ctx, c.stats)
	if err != nil {
		framework.Close()
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { framework.Run(); close(done) }()
	defer func() { framework.Close(); <-done }()
	listener := c.worker.quicListener
	trust, err := clientTLSConfig(cert)
	if err != nil {
		t.Fatal(err)
	}
	cfg := tunnel.ClientConfig{Key: d.server.Key, Password: "app-test-password", URL: "quic://" + listener.Addr().String() + "/tunnel", TLSConfig: trust}
	check := func(want string) {
		client, err := tunnel.NewClient(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if result := client.Probe(context.Background()); !result.OK || result.Target != want {
			t.Fatalf("probe=%+v want target=%s", result, want)
		}
	}
	check(backend)
	next := d
	server := *d.server
	server.Target = replacement
	next.server = &server
	if err := c.replaceShared(next); err != nil {
		t.Fatal(err)
	}
	if c.worker.quicListener != listener {
		t.Fatal("shared replacement recreated UDP listener")
	}
	check(replacement)
	next.quicListen = "127.0.0.1:1"
	if err := c.replaceShared(next); err == nil {
		t.Fatal("UDP listen change accepted without full restart")
	}
	check(replacement)
}

func TestQUICListenerFailureHealthAndRecovery(t *testing.T) {
	cert, key := managedCertificate(t)
	backend, _ := probeBackend(t)
	c := newController(t, "server", "target = "+backend+"\nquic-listen = 127.0.0.1:0\ntls-cert = "+cert+"\ntls-key = "+key+"\n")
	q := c.worker.quic
	q.mu.Lock()
	listener := q.listener
	address := q.health.Address
	q.mu.Unlock()
	listener.Close()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h := q.snapshot()
		if h.Recoveries > 0 && h.Healthy {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if h := q.snapshot(); !h.Healthy || h.Recoveries == 0 || h.Failures == 0 || h.Address != address {
		t.Fatalf("QUIC was not recovered: %+v", h)
	}
	trust, err := clientTLSConfig(cert)
	if err != nil {
		t.Fatal(err)
	}
	client, err := tunnel.NewClient(tunnel.ClientConfig{Key: c.current.server.Key, Password: "app-test-password", URL: "quic://" + address + "/tunnel", TLSConfig: trust})
	if err != nil {
		t.Fatal(err)
	}
	if result := client.Probe(context.Background()); !result.OK {
		t.Fatal(result)
	}
	if !c.status().(map[string]any)["healthy"].(bool) {
		t.Fatal("healthy listener reported degraded")
	}
	// Keep the failed port occupied to exercise degraded health and cancellable retries.
	q.mu.Lock()
	listener = q.listener
	q.mu.Unlock()
	listener.Close()
	blocker, err := net.ListenPacket("udp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if !q.snapshot().Healthy {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if c.status().(map[string]any)["healthy"].(bool) {
		t.Fatal("UDP failure was hidden by live TCP listener")
	}
	if err := c.worker.stop(); err != nil {
		t.Fatal(err)
	}
	c.worker = nil
	blocker.Close()
	time.Sleep(150 * time.Millisecond)
	if q.snapshot().State != "stopped" {
		t.Fatal("stopped worker attempted to restart")
	}
	rebound, err := net.ListenPacket("udp", address)
	if err != nil {
		t.Fatal("stopped recovery loop leaked UDP port", err)
	}
	rebound.Close()
}

func TestQUICListenerRecoveryPreservesBusinessSocket(t *testing.T) {
	cert, key := managedCertificate(t)
	backend := onlineBackend(t, "")
	c := newController(t, "server", "target = "+backend+"\nquic-listen = 127.0.0.1:0\ntls-cert = "+cert+"\ntls-key = "+key+"\n")
	address := c.worker.quic.snapshot().Address
	trust, err := clientTLSConfig(cert)
	if err != nil {
		t.Fatal(err)
	}
	client, err := tunnel.NewClient(tunnel.ClientConfig{Key: c.current.server.Key, Password: "app-test-password", URL: "quic://" + address + "/tunnel", TLSConfig: trust, ReconnectMin: 10 * time.Millisecond, ReconnectMax: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	local, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx, local) }()
	defer func() { cancel(); <-done }()
	conn, err := net.Dial("tcp", local.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	check := func(text string) {
		if _, err := io.WriteString(conn, text); err != nil {
			t.Fatal(err)
		}
		p := make([]byte, len(text))
		if _, err := io.ReadFull(conn, p); err != nil || string(p) != text {
			t.Fatalf("echo=%q error=%v", p, err)
		}
	}
	check("before listener failure")
	c.worker.quic.mu.Lock()
	listener := c.worker.quic.listener
	c.worker.quic.mu.Unlock()
	listener.Close()
	check("same business socket after UDP recovery")
	if c.worker.quic.snapshot().Recoveries == 0 {
		t.Fatal("test did not exercise listener recovery")
	}
}

func TestTransportBandwidthAttribution(t *testing.T) {
	cert, key := managedCertificate(t)
	backend := onlineBackend(t, "")
	c := newController(t, "server", "target = "+backend+"\nquic-listen = 127.0.0.1:0\ntls-cert = "+cert+"\ntls-key = "+key+"\n")
	trust, err := clientTLSConfig(cert)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"native", "legacy", "wss"} {
		t.Run(mode, func(t *testing.T) {
			stats := tunnel.NewStats()
			endpoint := "quic://" + c.worker.quicListener.Addr().String() + "/tunnel"
			if mode == "wss" {
				endpoint = "wss://" + c.worker.listener.Addr().String() + "/tunnel"
			}
			clientMode := mode
			if mode == "wss" {
				clientMode = "native"
			}
			client, err := tunnel.NewClient(tunnel.ClientConfig{Key: c.current.server.Key, Password: "app-test-password", URL: endpoint, TLSConfig: trust, QUICMode: clientMode, Options: tunnel.Options{Stats: stats}})
			if err != nil {
				t.Fatal(err)
			}
			local, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- client.Run(ctx, local) }()
			defer func() {
				cancel()
				if err := <-done; err != nil {
					t.Error(err)
				}
			}()
			app, err := net.Dial("tcp", local.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer app.Close()
			app.SetDeadline(time.Now().Add(5 * time.Second))
			payload := "transport accounting echo"
			if _, err = io.WriteString(app, payload); err != nil {
				t.Fatal(err)
			}
			response := make([]byte, len(payload))
			if _, err = io.ReadFull(app, response); err != nil || string(response) != payload {
				t.Fatal("echo failed", err)
			}
			snapshot := stats.Snapshot()
			selected, other := snapshot.Transports.QUIC, snapshot.Transports.TCP
			if mode == "wss" {
				selected, other = other, selected
			}
			if selected.TX == 0 || selected.RX == 0 || other != (tunnel.TransportBytesSnapshot{}) {
				t.Fatalf("incorrect client classification: %+v", snapshot.Transports)
			}
			if len(snapshot.ByIP) != 1 {
				t.Fatal("missing peer counters", snapshot.ByIP)
			}
			selected, other = snapshot.ByIP[0].Transports.QUIC, snapshot.ByIP[0].Transports.TCP
			if mode == "wss" {
				selected, other = other, selected
			}
			if selected.TX == 0 || selected.RX == 0 || other != (tunnel.TransportBytesSnapshot{}) {
				t.Fatalf("incorrect peer classification: %+v", snapshot.ByIP)
			}
		})
	}
	snapshot := c.stats.Snapshot()
	if snapshot.Transports.TCP.TX == 0 || snapshot.Transports.TCP.RX == 0 || snapshot.Transports.QUIC.TX == 0 || snapshot.Transports.QUIC.RX == 0 {
		t.Fatalf("server did not separate traffic: %+v", snapshot.Transports)
	}
}
