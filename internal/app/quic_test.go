package app

import (
	"context"
	mastergin "github.com/acl-dev/gin-service"
	"github.com/gin-gonic/gin"
	"net"
	"testing"

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
	c := newController(t, "server", "listen = "+onlineAddress(t)+"\ntarget = "+backend+"\nquic-listen = 127.0.0.1:0\ntls-cert = "+cert+"\ntls-key = "+key+"\n")
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
