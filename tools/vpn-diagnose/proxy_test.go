package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestGoogleProxyDomainBoundary(t *testing.T) {
	for _, host := range []string{"mail.google.com", "google.com", "fonts.gstatic.com", "a.googleapis.com", "a.googleusercontent.com"} {
		if !googleHost(host) {
			t.Fatal(host)
		}
	}
	for _, host := range []string{"google.com.evil.example", "evilgoogle.com", "127.0.0.1", "localhost", "example.com", ""} {
		if googleHost(host) {
			t.Fatal(host)
		}
	}
}
func TestBrowserProxyRefusesExternalBind(t *testing.T) {
	dial := func(context.Context, string, string) (net.Conn, error) { t.Fatal("unexpected dial"); return nil, nil }
	for _, address := range []string{"0.0.0.0:0", "[::]:0", "localhost:0", ":0"} {
		err := serveBrowserProxy(context.Background(), address, dial, map[string]any{}, func() {})
		if err == nil || !strings.Contains(err.Error(), "127.0.0.1") {
			t.Fatalf("%s: %v", address, err)
		}
	}
}

func TestProxyPinsIPv4FromSelectedResolver(t *testing.T) {
	calls := 0
	lookup := func(context.Context, string) ([]string, error) {
		calls++
		return []string{"2001:db8::1", "192.0.2.10"}, nil
	}
	var destinations []string
	dial := resolvedProxyDial(func(_ context.Context, network, address string) (net.Conn, error) {
		if network != "tcp4" {
			t.Fatal(network)
		}
		destinations = append(destinations, address)
		return nil, nil
	}, lookup)
	for i := 0; i < 2; i++ {
		if _, err := dial(context.Background(), "tcp4", "mail.google.com:443"); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 || len(destinations) != 2 || destinations[0] != "192.0.2.10:443" || destinations[1] != destinations[0] {
		t.Fatal(calls, destinations)
	}
}

func TestBrowserProxyPreservesOpaqueBytesAndClosesOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	report := map[string]any{}
	ready := make(chan string, 1)
	var once sync.Once
	save := func() { once.Do(func() { ready <- report["proxy_address"].(string) }) }
	dial := func(context.Context, string, string) (net.Conn, error) {
		a, b := net.Pipe()
		go func() { defer b.Close(); io.Copy(b, b) }()
		return a, nil
	}
	done := make(chan error, 1)
	go func() { done <- serveBrowserProxy(ctx, "127.0.0.1:0", dial, report, save) }()
	var address string
	select {
	case address = <-ready:
	case err := <-done:
		t.Fatal(err)
	case <-time.After(3 * time.Second):
		t.Fatal("startup stalled")
	}
	conn, err := net.DialTimeout("tcp4", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	fmt.Fprint(conn, "CONNECT mail.google.com:443 HTTP/1.1\r\nHost: mail.google.com:443\r\n\r\n")
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: "CONNECT"})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 {
		t.Fatal(response.Status)
	}
	opaque := []byte("opaque test payload never recorded")
	if _, err = conn.Write(opaque); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(opaque))
	if _, err = io.ReadFull(reader, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, opaque) {
		t.Fatal("proxy changed payload")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("proxy did not close active tunnel")
	}
	data, _ := json.Marshal(report)
	if bytes.Contains(data, opaque) {
		t.Fatal("report retained payload")
	}
	entries := report["connections"].([]map[string]any)
	if len(entries) != 1 || entries[0]["rx_bytes"] != int64(len(opaque)) {
		t.Fatal(entries)
	}
}
