package master_gin

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestRawTCPLeaseNeverServesHTTPAndCanBeReplaced(t *testing.T) {
	s, err := Init("127.0.0.1:0,127.0.0.1:0", WithContext(context.Background()), WithManaged(false))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	addresses := s.Addresses()
	raw, err := s.LeaseTCP(addresses[0])
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := s.LeaseTCP(addresses[0]); err == nil {
		t.Fatal("duplicate lease accepted")
	}
	if _, err := s.LeaseTCP("127.0.0.1:1"); err == nil {
		t.Fatal("missing inherited listener accepted")
	}
	s.GET("/*path", gin.WrapH(s.Handler()))
	worker, err := s.Configure(ServerConfig{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "http-only") })})
	if err != nil {
		t.Fatal(err)
	}
	if len(worker.listeners) != 1 || worker.PrimaryListener().Addr().String() != addresses[1] {
		t.Fatal("raw socket was assigned HTTP")
	}
	done := make(chan struct{})
	go func() { s.Run(); close(done) }()
	defer func() { s.Close(); <-done }()
	for generation := 0; generation < 3; generation++ {
		echoed := make(chan error, 1)
		go func(l net.Listener) {
			c, err := l.Accept()
			if err == nil {
				c.SetDeadline(time.Now().Add(time.Second))
				_, err = io.CopyN(c, c, 3)
				c.Close()
			}
			echoed <- err
		}(raw)
		c, err := net.DialTimeout("tcp", addresses[0], time.Second)
		if err != nil {
			t.Fatal(err)
		}
		c.SetDeadline(time.Now().Add(time.Second))
		c.Write([]byte{0, 255, 17})
		b := make([]byte, 3)
		_, err = io.ReadFull(c, b)
		c.Close()
		if err != nil || b[0] != 0 || b[1] != 255 || b[2] != 17 {
			t.Fatal(b, err)
		}
		if err := <-echoed; err != nil {
			t.Fatal(err)
		}
		raw.Close()
		raw, err = s.LeaseTCP(addresses[0])
		if err != nil {
			t.Fatal(err)
		}
	}
	defer raw.Close()
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get("http://" + addresses[1] + "/test")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || string(data) != "http-only" {
		t.Fatal(string(data), err)
	}
}

func TestUnclaimedInheritedPortCannotExposeHTTP(t *testing.T) {
	s, err := Init("127.0.0.1:0,127.0.0.1:0", WithContext(context.Background()), WithManaged(false))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Alone = false
	addresses := s.Addresses()
	if _, err := s.Configure(ServerConfig{Addresses: addresses[0]}); err == nil {
		t.Fatal("unclaimed inherited TCP port became HTTP")
	}
	raw, err := s.LeaseTCP(addresses[1])
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	worker, err := s.Configure(ServerConfig{Addresses: addresses[0]})
	if err != nil {
		t.Fatal(err)
	}
	if len(worker.listeners) != 1 {
		t.Fatal("HTTP exposed raw TCP port")
	}
}

func TestInheritedRawPortExpansionKeepsConfiguredBindScope(t *testing.T) {
	ipv6, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skip("IPv6 loopback unavailable:", err)
	}
	_, port, _ := net.SplitHostPort(ipv6.Addr().String())
	ipv6.Close()
	rawAddress := "[::1]:" + port
	extraAddress := "127.0.0.1:" + port
	s, err := Init("127.0.0.1:0,"+extraAddress+","+rawAddress, WithContext(context.Background()), WithManaged(false))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Alone = false
	raw, err := s.LeaseTCP(rawAddress)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	s.GET("/*path", gin.WrapH(s.Handler()))
	worker, err := s.Configure(ServerConfig{Addresses: s.Addresses()[0], Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "http-only") })})
	if err != nil {
		t.Fatal(err)
	}
	if len(worker.listeners) != 1 {
		t.Fatalf("HTTP claimed reverse port: %d listeners", len(worker.listeners))
	}
	lease := raw.(*tcpLease)
	if len(lease.listeners) != 1 || lease.listeners[0].Addr().String() != rawAddress {
		t.Fatal("reverse mapping expanded beyond configured IP")
	}
	conn, err := net.DialTimeout("tcp", extraAddress, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(time.Second))
	conn.Write([]byte("GET / HTTP/1.1\r\nHost: test\r\n\r\n"))
	var b [1]byte
	n, err := conn.Read(b[:])
	if n != 0 || err == nil {
		t.Fatal("unmapped interface returned data", n, err)
	}
	if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("unmapped interface did not close the connection", err)
	}
}
