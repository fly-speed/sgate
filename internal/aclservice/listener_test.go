package aclservice

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestInheritedListenersLeaseRestartAndShutdown(t *testing.T) {
	var listeners []net.Listener
	for i := 0; i < 2; i++ {
		l, e := net.Listen("tcp", "127.0.0.1:0")
		if e != nil {
			t.Fatal(e)
		}
		listeners = append(listeners, l)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := NewPool(listeners, cancel)
	defer p.Close()
	for round := 0; round < 20; round++ {
		lease, e := p.Lease()
		if e != nil {
			t.Fatal(e)
		}
		for _, l := range listeners {
			c, e := net.DialTimeout("tcp", l.Addr().String(), time.Second)
			if e != nil {
				t.Fatal(e)
			}
			c.SetDeadline(time.Now().Add(time.Second))
			accepted := make(chan net.Conn, 1)
			go func() { c, _ := lease.Accept(); accepted <- c }()
			peer := <-accepted
			if peer == nil {
				t.Fatal("accepted nil")
			}
			peer.SetDeadline(time.Now().Add(time.Second))
			done := make(chan error, 1)
			go func() { _, e := peer.Write([]byte{byte(round)}); peer.Close(); done <- e }()
			b := make([]byte, 1)
			if _, e := io.ReadFull(c, b); e != nil || b[0] != byte(round) {
				t.Fatal("wrong generation", e)
			}
			c.Close()
			if e := <-done; e != nil {
				t.Fatal(e)
			}
		}
		lease.Close()
		if _, e := lease.Accept(); !errors.Is(e, net.ErrClosed) {
			t.Fatal("old lease can still accept", e)
		}
		select {
		case <-ctx.Done():
			t.Fatal("worker restart canceled framework")
		default:
		}
	}
	lease, e := p.Lease()
	if e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { _, e := lease.Accept(); done <- e }()
	p.Close()
	select {
	case e := <-done:
		if !errors.Is(e, net.ErrClosed) {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("accept blocked after framework shutdown")
	}
	if _, e := p.Lease(); !errors.Is(e, net.ErrClosed) {
		t.Fatal("new lease after shutdown")
	}
	for _, l := range listeners {
		rebound, e := net.Listen("tcp", l.Addr().String())
		if e != nil {
			t.Fatal("inherited socket leaked", e)
		}
		rebound.Close()
	}
}
