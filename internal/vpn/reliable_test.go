package vpn

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/netip"
	"testing"
	"time"
)

func TestReliableVPNTransportsDuplexProbeReconnectAndNoise(t *testing.T) {
	for _, wire := range []string{"tcp", "wss"} {
		t.Run(wire, func(t *testing.T) {
			server, cfg, remote := fixtureWire(t, 4, NoiseConfig{Rate: 1024, Idle: true}, wire)
			probe := Probe(context.Background(), cfg)
			if !probe.OK || probe.AddressInUse {
				t.Fatalf("probe: %+v", probe)
			}
			cfg.Noise = NoiseConfig{Rate: 1024, Idle: true}
			local, _, _ := startClient(t, cfg)
			await(t, "authenticated reliable VPN", func() bool { return cfg.Stats.Snapshot().Active == 1 })
			occupied := Probe(context.Background(), cfg)
			if !occupied.OK || !occupied.AddressInUse {
				t.Fatalf("occupied probe: %+v", occupied)
			}
			for _, size := range []int{60, 1000, 1280} {
				outgoing := ipPacket("10.77.0.2", "8.8.8.8", size)
				local.out <- outgoing
				receivePacket(t, remote, outgoing)
				incoming := ipPacket("8.8.8.8", "10.77.0.2", size)
				remote.out <- incoming
				receivePacket(t, local, incoming)
			}
			await(t, "duplex reliable noise", func() bool { s := cfg.Stats.Snapshot(); return s.NoiseRX > 0 && s.NoiseTX > 0 })
			server.mu.Lock()
			p := server.peers[cfg.ClientIP]
			server.mu.Unlock()
			p.conn.CloseWithError(0, "test reconnect")
			await(t, "reliable reconnect", func() bool { return cfg.Stats.Snapshot().Active == 1 && cfg.Stats.Snapshot().Connections >= 2 })
			outgoing := ipPacket("10.77.0.2", "8.8.4.4", 1280)
			local.out <- outgoing
			receivePacket(t, remote, outgoing)
			if cfg.Stats.Snapshot().QueueOverflow != 0 {
				t.Fatal("unexpected queue loss")
			}
		})
	}
}
func TestReliableVPNRejectsUntrustedCertificateAndWrongKey(t *testing.T) {
	for _, wire := range []string{"tcp", "wss"} {
		t.Run(wire, func(t *testing.T) {
			_, cfg, _ := fixtureWire(t, 4, NoiseConfig{}, wire)
			bad := cfg
			bad.TLS = &tls.Config{ServerName: "127.0.0.1"}
			result := Probe(context.Background(), bad)
			if result.OK || result.Stage != "tls" {
				t.Fatalf("untrusted certificate accepted: %+v", result)
			}
			bad = cfg
			bad.Key = bytes.Repeat([]byte{77}, 32)
			result = Probe(context.Background(), bad)
			if result.OK || result.Stage != "auth" {
				t.Fatalf("wrong key accepted: %+v", result)
			}
		})
	}
}
func TestReliableRecordShutdownUnblocksUnreadControlAndData(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	c := newRecordConnection(a)
	// The receiver cannot accept this control byte until OpenStream is read.
	go func() { b.Write([]byte{recordControl, 0, 1, 1}) }()
	done := make(chan struct{})
	go func() { c.CloseWithError(0, "stopped"); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stalled control blocked shutdown")
	}
}

func TestMixedVPNListenersShareAuthenticationAndVirtualAddressSpace(t *testing.T) {
	server, cfg, remote := fixtureWire(t, 4, NoiseConfig{}, "mixed")
	for i, wire := range []string{"quic", "tcp", "wss"} {
		c := cfg
		c.Transport = wire
		c.ClientIP = netip.MustParseAddr(fmt.Sprintf("10.77.0.%d", i+2))
		c.Stats = &Stats{}
		if wire != "quic" {
			c.Address = server.TCPAddr().String()
			c.Host = c.Address
		}
		local, _, _ := startClient(t, c)
		await(t, "mixed "+wire, func() bool { return c.Stats.Snapshot().Active == 1 })
		outgoing := ipPacket(c.ClientIP.String(), "8.8.8.8", 1280)
		local.out <- outgoing
		receivePacket(t, remote, outgoing)
		incoming := ipPacket("8.8.8.8", c.ClientIP.String(), 1280)
		remote.out <- incoming
		receivePacket(t, local, incoming)
	}
	await(t, "all three transports", func() bool { return server.cfg.Stats.Snapshot().Active == 3 })
	c := cfg
	c.ClientIP = netip.MustParseAddr("10.77.0.3")
	result := Probe(context.Background(), c)
	if !result.OK || !result.AddressInUse {
		t.Fatalf("QUIC did not see TCP virtual address: %+v", result)
	}
}

func TestReliableRecordShutdownUnblocksCongestedWriter(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	c := newRecordConnection(a)
	writeDone := make(chan error, 1)
	go func() { writeDone <- c.SendDatagram(make([]byte, 1000)) }()
	stopped := make(chan struct{})
	go func() { c.CloseWithError(0, "stop"); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("data write blocked shutdown")
	}
	select {
	case <-writeDone:
	case <-time.After(time.Second):
		t.Fatal("writer did not exit")
	}
}
func TestReliableRecordRejectsOversizedFramesBeforeReadingPayload(t *testing.T) {
	for _, header := range [][]byte{{recordControl, 0xff, 0xff}, {recordPacket, 0x20, 0}} {
		a, b := net.Pipe()
		c := newRecordConnection(a)
		sent := make(chan struct{})
		go func() { b.Write(header); close(sent) }()
		select {
		case <-c.Context().Done():
		case <-time.After(time.Second):
			t.Fatal("oversized record not rejected")
		}
		c.CloseWithError(0, "done")
		b.Close()
		<-sent
	}
}

func TestReliableIPUsesWholePacketRecordsWithoutDatagramReassembly(t *testing.T) {
	a, b := net.Pipe()
	sender, receiver := newRecordConnection(a), newRecordConnection(b)
	defer sender.CloseWithError(0, "done")
	defer receiver.CloseWithError(0, "done")
	packet := ipPacket("8.8.8.8", "10.77.0.2", 1500)
	if err := sender.sendIP(packet); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := receiver.ReceiveDatagram(ctx)
	if err != nil || len(got) != 1502 || !bytes.Equal(got[:2], []byte{2, 0}) || !bytes.Equal(got[2:], packet) {
		t.Fatal("whole IP frame not preserved", len(got), err)
	}
}

func TestStableClientIdentityReplacesStaleSessionAcrossTransports(t *testing.T) {
	server, cfg, remote := fixtureWire(t, 1, NoiseConfig{}, "mixed")
	id, err := NewClientIdentity()
	if err != nil {
		t.Fatal(err)
	}
	cfg.ClientID = id
	cfg.ClientIP = netip.Addr{}
	assigned := make(chan netip.Addr, 2)
	cfg.Assigned = func(ip netip.Addr) { assigned <- ip }
	cfg.RetryMin = 5 * time.Second
	cfg.RetryMax = 5 * time.Second
	_, stopOld, _ := startClient(t, cfg)
	await(t, "old QUIC client", func() bool { return cfg.Stats.Snapshot().Active == 1 })
	oldIP := waitAssignment(t, assigned)
	next := cfg
	next.Transport = "wss"
	next.Address = server.TCPAddr().String()
	next.Host = next.Address
	next.Stats = &Stats{}
	local, _, _ := startClient(t, next)
	await(t, "WSS replaces stale lease", func() bool { return next.Stats.Snapshot().Active == 1 })
	newIP := waitAssignment(t, assigned)
	if newIP != oldIP {
		t.Fatal("protocol switch lost automatic assignment", oldIP, newIP)
	}
	stopOld()
	await(t, "one current lease", func() bool { return server.cfg.Stats.Snapshot().Active == 1 })
	packet := ipPacket(newIP.String(), "8.8.8.8", 1280)
	local.out <- packet
	receivePacket(t, remote, packet)
}
