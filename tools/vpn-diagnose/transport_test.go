package main

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
	"net/netip"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/tun/netstack"
	"sgate/internal/vpn"
)

// Real inner TCP and HTTP run in userspace stacks on both ends. Only the outer
// VPN listeners use loopback sockets; no privileged TUN, DNS or routes change.
func TestVPNTransportsCompleteLargeHTTPResource(t *testing.T) {
	for _, wire := range []string{"quic", "tcp", "wss", "udp"} {
		t.Run(wire, func(t *testing.T) {
			seed := httptest.NewTLSServer(nil)
			roots := x509.NewCertPool()
			roots.AddCert(seed.Certificate())
			serverTLS := seed.TLS
			seed.Close()
			gateway := netip.MustParseAddr("10.77.0.1")
			clientIP := netip.MustParseAddr("10.77.0.2")
			serverRaw, serverStack, err := netstack.CreateNetTUN([]netip.Addr{gateway}, nil, 1280)
			if err != nil {
				t.Fatal(err)
			}
			remote := &device{Device: serverRaw}
			t.Cleanup(func() { remote.Close() })
			listener, err := serverStack.ListenTCP(&net.TCPAddr{IP: net.ParseIP(gateway.String()), Port: 18080})
			if err != nil {
				t.Fatal(err)
			}
			payload := bytes.Repeat([]byte("Gmail-like public JavaScript resource integrity.\n"), 45000)
			web := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body := payload
				if r.Method == "POST" {
					var e error
					body, e = io.ReadAll(io.LimitReader(r.Body, 65537))
					if e != nil || len(body) > 65536 {
						http.Error(w, "invalid upload", 400)
						return
					}
				}
				w.Header().Set("Content-Length", fmt.Sprint(len(body)))
				w.Write(body)
			})}
			go web.Serve(listener)
			defer web.Close()
			config := vpn.Config{Address: "127.0.0.1:0", Key: bytes.Repeat([]byte{32}, 32), TLS: serverTLS, MTU: 1280, Network: netip.MustParsePrefix("10.77.0.0/24")}
			if wire == "udp" {
				config.Address = ""
				config.UDPListen = "127.0.0.1:0"
			} else if wire != "quic" {
				config.Address = ""
				config.TCPListen = "127.0.0.1:0"
			}
			server, err := vpn.ListenServer(config, remote)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			serverDone := make(chan error, 1)
			go func() { serverDone <- server.Serve(ctx) }()
			t.Cleanup(func() {
				cancel()
				server.Close()
				select {
				case e := <-serverDone:
					if e != nil {
						t.Error(e)
					}
				case <-time.After(5 * time.Second):
					t.Error("server did not stop")
				}
			})
			clientRaw, clientStack, err := netstack.CreateNetTUN([]netip.Addr{clientIP}, nil, 1280)
			if err != nil {
				t.Fatal(err)
			}
			local := &device{Device: clientRaw}
			config.Address = server.Addr().String()
			config.Host = config.Address
			config.TCPListen = ""
			config.Transport = wire
			config.ClientIP = clientIP
			config.Stats = &vpn.Stats{}
			config.TLS = &tls.Config{RootCAs: roots, ServerName: "127.0.0.1"}
			clientDone := make(chan error, 1)
			go func() { clientDone <- vpn.RunClient(ctx, config, local) }()
			t.Cleanup(func() {
				cancel()
				local.Close()
				select {
				case e := <-clientDone:
					if e != nil {
						t.Error(e)
					}
				case <-time.After(5 * time.Second):
					t.Error("client did not stop")
				}
			})
			until := time.Now().Add(5 * time.Second)
			for config.Stats.Snapshot().Active == 0 {
				if time.Now().After(until) {
					t.Fatal("VPN authentication timed out")
				}
				time.Sleep(10 * time.Millisecond)
			}
			transport := &http.Transport{Proxy: nil, DialContext: clientStack.DialContext}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: 30 * time.Second}
			response, err := client.Get("http://10.77.0.1:18080/script.js")
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || response.StatusCode != 200 || !bytes.Equal(body, payload) {
				t.Fatal("large resource incomplete", len(body), err)
			}
			upload := bytes.Repeat([]byte{11}, 65536)
			response, err = client.Post("http://10.77.0.1:18080/upload", "application/octet-stream", bytes.NewReader(upload))
			if err != nil {
				t.Fatal(err)
			}
			body, err = io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || !bytes.Equal(body, upload) {
				t.Fatal("upload/echo mismatch", len(body), err)
			}
		})
	}
}
