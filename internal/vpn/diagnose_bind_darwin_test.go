//go:build darwin

package vpn

import (
	"net"
	"testing"
	"time"
)

func TestVPNDiagnosticBindsSocketToRequestedInterface(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			conn.Close()
		}
		accepted <- err
	}()
	dialer := &net.Dialer{Timeout: time.Second, LocalAddr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1")}}
	if err := bindDiagnosticInterface(dialer, "lo0"); err != nil {
		t.Fatal(err)
	}
	conn, err := dialer.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if err := <-accepted; err != nil {
		t.Fatal(err)
	}
	if err := bindDiagnosticInterface(dialer, "sgate-no-such-interface"); err == nil {
		t.Fatal("invalid interface silently accepted")
	}
}
