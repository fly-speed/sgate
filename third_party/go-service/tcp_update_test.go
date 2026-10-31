package master

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

func TestTCPOnlineAddressesKeepWorkerAndRollback(t *testing.T) {
	s, w, _ := tcpFixture(t)
	go func() {
		for {
			c, err := w.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	old := w.Addr().String()
	app, err := net.Dial("tcp", old)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	exchange := func() {
		t.Helper()
		app.SetDeadline(time.Now().Add(time.Second))
		app.Write([]byte("echo"))
		b := make([]byte, 4)
		if _, err := io.ReadFull(app, b); err != nil || string(b) != "echo" {
			t.Fatal("old connection lost", string(b), err)
		}
	}
	exchange()
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	added := free.Addr().String()
	free.Close()
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	if err = s.UpdateAddresses(old + "," + added + "," + occupied.Addr().String()); err == nil {
		t.Fatal("bound occupied port")
	}
	exchange()
	probe, err := net.Listen("tcp", added)
	if err != nil {
		t.Fatal("failed transaction leaked listener", err)
	}
	probe.Close()
	if err = s.UpdateAddresses(old + "," + added); err != nil {
		t.Fatal(err)
	}
	exchange()
	peer, err := net.Dial("tcp", added)
	if err != nil {
		t.Fatal(err)
	}
	peer.Close()
	if err = s.UpdateAddresses(old); err != nil {
		t.Fatal(err)
	}
	exchange()
	probe, err = net.Listen("tcp", added)
	if err != nil {
		t.Fatal("removed port still bound", err)
	}
	probe.Close()
	if len(s.Addresses()) != 1 {
		t.Fatal(s.Addresses())
	}
}

func TestTCPOnlineWildcardIdentity(t *testing.T) {
	s, err := TcpServiceInit("0.0.0.0:0", WithTCPContext(context.Background()), WithTCPManaged(false))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.AcceptHandler = s.AcceptConn
	w, err := s.Configure("0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	_, port, _ := net.SplitHostPort(w.Addr().String())
	if err := s.UpdateAddresses("0.0.0.0:" + port); err != nil {
		t.Fatal("wildcard listener was unnecessarily rebound", err)
	}
}
