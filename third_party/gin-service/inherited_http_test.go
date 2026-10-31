package master_gin

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestInheritedHTTPMatchesExpandedServicePort(t *testing.T) {
	// Addresses reported by acl_master on macOS, including scoped link-local IPs.
	bound := []string{"127.0.0.1:8081", "[::1]:8081", "[fe80::1%lo0]:8081", "[fe80::4d1:bff6:2d62:1ed7%en0]:8081", "192.168.1.22:8081", "[fe80::10dd:c8ff:fedd:30a2%awdl0]:8081", "[fe80::10dd:c8ff:fedd:30a2%llw0]:8081", "[fe80::bc99:1ecd:cac1:d9dc%utun0]:8081", "10.8.0.6:8081"}
	for _, configured := range []string{"127.0.0.1:8081", ":8081", "0.0.0.0:8081", "[::]:8081", "|8081"} {
		for _, address := range bound {
			if !matchesHTTPAddress(configured, address, true) {
				t.Errorf("managed %s rejected %s", configured, address)
			}
			other := strings.TrimSuffix(address, ":8081") + ":10022"
			if matchesHTTPAddress(configured, other, true) {
				t.Errorf("unreserved reverse port became HTTP: %s", other)
			}
		}
	}
	if matchesHTTPAddress("127.0.0.1:0", "[::1]:10022", true) {
		t.Fatal("managed port zero admitted arbitrary service ports")
	}
	if matchesHTTPAddress("127.0.0.1:8081", "[::1]:8081", false) {
		t.Fatal("standalone bind IP matching changed")
	}
}

func TestInheritedHTTPStartsAcrossIPv4AndIPv6(t *testing.T) {
	probe, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skip("IPv6 loopback unavailable:", err)
	}
	_, port, _ := net.SplitHostPort(probe.Addr().String())
	probe.Close()
	ipv4, ipv6 := "127.0.0.1:"+port, "[::1]:"+port
	s, err := Init(ipv4+","+ipv6+",127.0.0.1:0", WithContext(context.Background()), WithManaged(false))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// Init has acquired real listeners. Configure now follows the inherited path.
	s.Alone = false
	addresses := s.Addresses()
	if _, err = s.Configure(ServerConfig{Addresses: ipv4}); err == nil {
		t.Fatal("unreserved extra port became HTTP")
	}
	raw, err := s.LeaseTCP(addresses[2])
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	s.GET("/*path", gin.WrapH(s.Handler()))
	worker, err := s.Configure(ServerConfig{Addresses: ipv4, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ready") })})
	if err != nil {
		t.Fatal(err)
	}
	if len(worker.listeners) != 2 {
		t.Fatalf("HTTP listener count %d, want both IPv4 and IPv6", len(worker.listeners))
	}
	done := make(chan struct{})
	go func() { s.Run(); close(done) }()
	defer func() { s.Close(); <-done }()
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
	for _, address := range []string{ipv4, ipv6} {
		response, err := client.Get("http://" + address + "/ready")
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || string(body) != "ready" {
			t.Fatal(address, string(body), err)
		}
	}
}
