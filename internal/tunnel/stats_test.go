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
	if v.TX != 8 || v.RX != 8 || v.Connections != 1 || v.Transports.TCP.TX != 8 || v.Transports.TCP.RX != 8 || v.Transports.QUIC != (TransportBytesSnapshot{}) {
		t.Fatal(v)
	}
	c.Close()
	c.Close()
	b.Close()
	native := newNativeTraffic(stats, a.RemoteAddr())
	native.tx(7)
	native.rx(11)
	native.close()
	native.close()
	mixed := stats.Snapshot()
	if mixed.TX != 15 || mixed.RX != 19 || mixed.Transports.TCP != v.Transports.TCP || mixed.Transports.QUIC != (TransportBytesSnapshot{TX: 7, RX: 11}) {
		t.Fatal("mixed transport accounting lost or duplicated bytes", mixed)
	}
	if len(mixed.ByIP) != 1 || mixed.ByIP[0].Transports != mixed.Transports {
		t.Fatal("mixed protocols on the same peer IP were not separated", mixed.ByIP)
	}
	if stats.Snapshot().Connections != 0 {
		t.Fatal("connection count leaked")
	}
}
