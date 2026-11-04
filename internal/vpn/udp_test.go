package vpn

import (
	"bytes"
	"context"
	"flag"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestUDPOnlySettingsDoNotRequireCertificates(t *testing.T) {
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	RegisterFlags(fs, "server")
	key := filepath.Join(t.TempDir(), "key")
	os.WriteFile(key, []byte(strings.Repeat("01", 32)), 0600)
	fs.String("key-file", key, "")
	fs.Set("vpn-mode", "only")
	fs.Set("vpn-listen", "")
	fs.Set("vpn-udp-listen", "127.0.0.1:8445")
	p, err := Prepare("server", fs)
	if err != nil || p == nil || p.Config.UDPListen != "127.0.0.1:8445" {
		t.Fatal(p, err)
	}
	fs.Set("vpn-listen", "127.0.0.1:8445")
	if _, err = Prepare("server", fs); err == nil {
		t.Fatal("QUIC enabled without certificate or with conflicting UDP port")
	}
	client := integratedSettings(t)
	client.Set("vpn-mode", "only")
	client.Set("vpn-server", "udp://vpn.invalid:8445/vpn")
	client.Set("vpn-tls-ca", "missing-unused.pem")
	p, err = Prepare("client", client)
	if err != nil || p.Config.Transport != "udp" {
		t.Fatal(p, err)
	}
}

func TestUDPFrameValidation(t *testing.T) {
	valid, err := encodeUDPFrame(udpPacket, []byte("packet"))
	if err != nil {
		t.Fatal(err)
	}
	k, p, err := decodeUDPFrame(valid)
	if err != nil || k != udpPacket || string(p) != "packet" {
		t.Fatal(k, err)
	}
	for _, change := range []func([]byte){func(b []byte) { b[3] = 2 }, func(b []byte) { b[4] = 99 }, func(b []byte) { b[5] = 1 }, func(b []byte) { b[7]++ }} {
		b := append([]byte(nil), valid...)
		change(b)
		if _, _, err := decodeUDPFrame(b); err == nil {
			t.Fatal("invalid header accepted", b[:8])
		}
	}
	if _, err := encodeUDPFrame(udpPacket, make([]byte, udpFrameMax)); err == nil {
		t.Fatal("oversize frame accepted")
	}
}

func TestUDPDataLossIsNotRetransmittedAndReplayIsRejected(t *testing.T) {
	s, c, remote := fixtureWire(t, 4, NoiseConfig{}, "udp")
	relay, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := s.UDPAddr()
	var client net.Addr
	var drop atomic.Bool
	var writes atomic.Int64
	captured := make(chan []byte, 1)
	relayDone := make(chan struct{})
	go func() {
		defer close(relayDone)
		b := make([]byte, 65535)
		for {
			n, from, e := relay.ReadFrom(b)
			if e != nil {
				return
			}
			data := append([]byte(nil), b[:n]...)
			if from.String() == server.String() {
				if client != nil {
					relay.WriteTo(data, client)
				}
				continue
			}
			client = from
			// DTLS 1.2 encrypted application records have content type 23.
			if len(data) > 0 && data[0] == 23 {
				writes.Add(1)
				if drop.Swap(false) {
					captured <- data
					continue
				}
			}
			relay.WriteTo(data, server)
		}
	}()
	t.Cleanup(func() { relay.Close(); <-relayDone })
	c.Address = relay.LocalAddr().String()
	local, _, _ := startClient(t, c)
	await(t, "UDP authenticated through relay", func() bool { return c.Stats.Snapshot().Active == 1 })
	before := writes.Load()
	drop.Store(true)
	first := ipPacket(c.ClientIP.String(), "8.8.8.8", 100)
	local.out <- first
	var wire []byte
	select {
	case wire = <-captured:
	case <-time.After(time.Second):
		t.Fatal("data was not sent")
	}
	if bytes.Contains(wire, first) {
		t.Fatal("UDP payload sent without encryption")
	}
	select {
	case <-remote.in:
		t.Fatal("lost packet was replayed")
	case <-time.After(250 * time.Millisecond):
	}
	if writes.Load() != before+1 {
		t.Fatal("VPN retransmitted application records", writes.Load()-before)
	}
	second := ipPacket(c.ClientIP.String(), "8.8.4.4", 100)
	local.out <- second
	receivePacket(t, remote, second) // No head-of-line wait for the lost datagram.
	// Deliver the first encrypted record late, twice. Accept once despite order,
	// then reject its duplicate without delivering duplicate IP packets.
	relay.WriteTo(wire, server)
	receivePacket(t, remote, first)
	relay.WriteTo(wire, server)
	select {
	case <-remote.in:
		t.Fatal("encrypted replay accepted")
	case <-time.After(150 * time.Millisecond):
	}
	// Corrupt a fresh ciphertext. The original must still be accepted afterwards.
	drop.Store(true)
	third := ipPacket(c.ClientIP.String(), "1.1.1.1", 100)
	local.out <- third
	select {
	case wire = <-captured:
	case <-time.After(time.Second):
		t.Fatal("third packet not sent")
	}
	bad := append([]byte(nil), wire...)
	bad[len(bad)-1] ^= 1
	relay.WriteTo(bad, server)
	select {
	case <-remote.in:
		t.Fatal("tampered data accepted")
	case <-time.After(150 * time.Millisecond):
	}
	relay.WriteTo(wire, server)
	receivePacket(t, remote, third)
}

func TestUDPWrongKeyCannotReserveAddress(t *testing.T) {
	s, c, _ := fixtureWire(t, 4, NoiseConfig{}, "udp")
	c.Key = bytes.Repeat([]byte{99}, 32)
	ctx, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
	defer cancel()
	r := Probe(ctx, c)
	if r.OK {
		t.Fatal("wrong UDP key accepted")
	}
	if len(s.Clients()) != 0 {
		t.Fatal("unauthenticated UDP peer acquired a lease")
	}
}

func TestUDPProbeNoiseAndCounters(t *testing.T) {
	_, c, _ := fixtureWire(t, 4, NoiseConfig{Rate: 1024, Idle: true}, "udp")
	c.Noise = NoiseConfig{Rate: 1024, Idle: true}
	c.ClientIP = netip.Addr{}
	r := Probe(context.Background(), c)
	if !r.OK || r.ClientIP == "" {
		t.Fatal(r)
	}
	startClient(t, c)
	await(t, "UDP padding and metrics", func() bool {
		q := c.Stats.Snapshot()
		return q.NoiseRX > 0 && q.NoiseTX > 0 && q.UDPTX > 0 && q.UDPRX > 0
	})
}

func TestUDPQUICAndTCPListenersShareAuthenticatedAddressPool(t *testing.T) {
	s, c, remote := fixtureWire(t, 4, NoiseConfig{}, "mixed")
	for _, wire := range []string{"quic", "udp", "tcp", "wss"} {
		cfg := c
		cfg.Transport = wire
		cfg.Stats = &Stats{}
		cfg.ClientIP = netip.Addr{}
		if wire == "udp" {
			cfg.Address = s.UDPAddr().String()
		} else if wire != "quic" {
			cfg.Address = s.TCPAddr().String()
		}
		cfg.Host = cfg.Address
		assigned := make(chan netip.Addr, 1)
		cfg.Assigned = func(ip netip.Addr) { assigned <- ip }
		local, _, _ := startClient(t, cfg)
		ip := waitAssignment(t, assigned)
		await(t, "mixed "+wire, func() bool { return cfg.Stats.Snapshot().Active == 1 })
		packet := ipPacket(ip.String(), "8.8.8.8", 100)
		local.out <- packet
		receivePacket(t, remote, packet)
	}
	clients := s.Clients()
	ips := map[string]bool{}
	protocols := map[string]bool{}
	for _, client := range clients {
		if ips[client.VPNIP] {
			t.Fatal("duplicate mixed-transport IP")
		}
		ips[client.VPNIP] = true
		protocols[client.Transport] = true
	}
	if len(ips) != 4 || len(protocols) != 4 {
		t.Fatal(clients)
	}
}
