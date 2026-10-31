package app

import (
	"bytes"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"sgate/internal/admin"
	"sgate/internal/tunnel"
)

func reverseAppConnect(t *testing.T, address, marker string) net.Conn {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err == nil {
			conn.SetDeadline(time.Now().Add(time.Second))
			b := make([]byte, len(marker))
			_, err = io.ReadFull(conn, b)
			if err == nil && string(b) == marker {
				conn.SetDeadline(time.Now().Add(5 * time.Second))
				t.Cleanup(func() { conn.Close() })
				return conn
			}
			conn.Close()
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("reverse client never served configured target")
	return nil
}

func TestReverseAccountManagementAndAppLifecycle(t *testing.T) {
	backend := onlineBackend(t, "R")
	server := newController(t, "server", "listen = "+onlineAddress(t)+"\ntarget = "+backend+"\n")
	snapshot, err := server.manageTunnelAccounts(nil)
	if err != nil {
		t.Fatal(err)
	}
	address := onlineAddress(t)
	mappings := []tunnel.ReverseMapping{{Listen: address, Target: backend}}
	snapshot, err = server.manageTunnelAccounts(&admin.TunnelAccountChange{Action: "create", Revision: snapshot.Revision, Username: "alice", Password: "alice-secret", ReverseMappings: &mappings})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Accounts[0].ReverseMappings) != 1 {
		t.Fatal("management omitted mappings")
	}
	newController(t, "client", "server = ws://"+server.worker.listener.Addr().String()+"/tunnel\nusername = alice\nallow-reverse = true\nreconnect-min = 10ms\nreconnect-max = 20ms\n", "-password", "alice-secret")
	conn := reverseAppConnect(t, address, "R")
	onlineExchange(t, conn)
	// Revalidating a running configuration must not try to bind its ports again.
	if _, err = server.load(""); err != nil {
		t.Fatal("validation rebound active port", err)
	}
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	previous, err := os.ReadFile(server.current.accountsFile)
	if err != nil {
		t.Fatal(err)
	}
	blocked := append(append([]tunnel.ReverseMapping{}, mappings...), tunnel.ReverseMapping{Listen: held.Addr().String(), Target: backend})
	if _, err = server.manageTunnelAccounts(&admin.TunnelAccountChange{Action: "update", Revision: snapshot.Revision, Username: "alice", ReverseMappings: &blocked}); err == nil {
		t.Fatal("occupied port update succeeded")
	}
	restored, _ := os.ReadFile(server.current.accountsFile)
	if !bytes.Equal(previous, restored) {
		t.Fatal("failed bind did not restore account document")
	}
	onlineExchange(t, conn)
	// Full standalone replacement releases and recreates listeners, then clients
	// reconnect without requiring forward-port traffic.
	if err = server.apply(); err != nil {
		t.Fatal(err)
	}
	conn = reverseAppConnect(t, address, "R")
	onlineExchange(t, conn)
	empty := []tunnel.ReverseMapping{}
	snapshot, err = server.manageTunnelAccounts(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = server.manageTunnelAccounts(&admin.TunnelAccountChange{Action: "update", Revision: snapshot.Revision, Username: "alice", ReverseMappings: &empty}); err != nil {
		t.Fatal(err)
	}
	if c, err := net.DialTimeout("tcp", address, 100*time.Millisecond); err == nil {
		c.Close()
		t.Fatal("cleared listener still bound")
	}
	if filepath.Base(server.current.accountsFile) != "tunnel-accounts.json" {
		t.Fatal("unexpected account file")
	}
}
