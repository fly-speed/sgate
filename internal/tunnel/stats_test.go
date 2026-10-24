package tunnel

import (
	"io"
	"net"
	"testing"
)

func TestTrafficStatsCountBytesAndCloseOnce(t *testing.T) {
	a, b := net.Pipe()
	stats := NewStats()
	c := stats.Conn(a)
	done := make(chan error, 1)
	go func() {
		_, e := b.Write([]byte("incoming"))
		if e == nil {
			buf := make([]byte, 8)
			_, e = io.ReadFull(b, buf)
		}
		done <- e
	}()
	buf := make([]byte, 8)
	if _, e := io.ReadFull(c, buf); e != nil {
		t.Fatal(e)
	}
	if _, e := c.Write([]byte("outgoing")); e != nil {
		t.Fatal(e)
	}
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	v := stats.Snapshot()
	if v.TX != 8 || v.RX != 8 || v.Connections != 1 {
		t.Fatal(v)
	}
	c.Close()
	c.Close()
	b.Close()
	if stats.Snapshot().Connections != 0 {
		t.Fatal("connection count leaked")
	}
}
