package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"sgate/internal/aclservice"
	"sgate/internal/admin"
	"sgate/internal/tunnel"
)

func probeBackend(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	count := &atomic.Int32{}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			count.Add(1)
			io.Copy(io.Discard, conn)
			conn.Close()
		}
	}()
	return listener.Addr().String(), count
}

func TestMappingProbeUsesEffectiveSettingsAndDraftAddresses(t *testing.T) {
	defaultBackend, defaultCount := probeBackend(t)
	selectedBackend, selectedCount := probeBackend(t)
	server, err := tunnel.NewServer(tunnel.ServerConfig{Key: bytes.Repeat([]byte{0x39}, 32), Password: "effective-cli-password",
		Target: defaultBackend, AllowedTargets: []string{selectedBackend}})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	web := httptest.NewServer(server)
	defer web.Close()
	endpoint := "ws" + strings.TrimPrefix(web.URL, "http") + "/tunnel"
	for _, managed := range []bool{false, true} {
		for _, multi := range []bool{false, true} {
			for _, send := range []bool{false, true} {
				t.Run(fmt.Sprintf("managed=%t/multi=%t/send=%t", managed, multi, send), func(t *testing.T) {
					text := "send-addr = true\n"
					if multi {
						text += "proxies = '[{\"listen\":\"127.0.0.1:9000\",\"server\":\"ws://unused.invalid/tunnel\"}]'\n"
					}
					c := proxyConfigController(t, text, "-password", "effective-cli-password", "-send-addr="+fmt.Sprint(send))
					if managed {
						c.managed = &aclservice.Pool{}
					}
					c.current, err = c.load("")
					if err != nil {
						t.Fatal(err)
					}
					before, _ := os.ReadFile(c.path)
					a, b := defaultCount.Load(), selectedCount.Load()
					result, err := c.managementView().TestProxy(context.Background(), admin.ProxyMapping{Listen: "127.0.0.1:9001", Server: endpoint, Addr: selectedBackend})
					if err != nil || !result.OK {
						t.Fatalf("probe: %+v %v", result, err)
					}
					waitManaged(t, "probe accepted by selected backend", func() bool {
						if send {
							return selectedCount.Load() == b+1 && defaultCount.Load() == a
						}
						return defaultCount.Load() == a+1 && selectedCount.Load() == b
					})
					after, _ := os.ReadFile(c.path)
					if !bytes.Equal(before, after) || c.generation != 0 || c.worker != nil {
						t.Fatal("probe saved config or started proxy")
					}
				})
			}
		}
	}
}
