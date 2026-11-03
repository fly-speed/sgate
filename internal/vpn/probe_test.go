package vpn

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"testing"
)

func TestVPNProbeAuthenticatesAndEchoesWithoutReplacingLiveClient(t *testing.T) {
	server, cfg, remote := fixture(t, 1)
	local, _, _ := startClient(t, cfg)
	await(t, "active client", func() bool { return cfg.Stats.Snapshot().Active == 1 })
	server.mu.Lock()
	original := server.peers[cfg.ClientIP]
	server.mu.Unlock()
	before := server.cfg.Stats.Snapshot()
	probe := cfg
	probe.Connected = func() (func(), error) { t.Error("probe changed host network"); return nil, nil }
	for i := 0; i < 3; i++ {
		result := Probe(context.Background(), probe)
		if !result.OK || result.Stage != "datagram" || !result.AddressInUse || !result.ClientLimitReached || result.TestID == "" {
			t.Fatal(result)
		}
	}
	server.mu.Lock()
	current := server.peers[cfg.ClientIP]
	server.mu.Unlock()
	if current != original || server.cfg.Stats.Snapshot().Connections != before.Connections || server.cfg.Stats.Snapshot().Active != 1 || len(remote.in) != 0 {
		t.Fatal("test changed live connection or wrote IP data")
	}
	packet := ipPacket("10.77.0.2", "8.8.8.8", 1280)
	local.out <- packet
	receivePacket(t, remote, packet)
}
func TestVPNProbeRejectsBadKeyCertificateAndNetworkWithoutTUN(t *testing.T) {
	_, cfg, remote := fixture(t, 1)
	for _, kind := range []string{"key", "certificate", "network"} {
		t.Run(kind, func(t *testing.T) {
			c := cfg
			wantStage := "auth"
			switch kind {
			case "key":
				c.Key = make([]byte, 32)
			case "certificate":
				c.TLS = &tls.Config{ServerName: "127.0.0.1"}
				wantStage = "quic"
			case "network":
				c.MTU = 1200
			}
			result := Probe(context.Background(), c)
			if result.OK || result.Stage != wantStage || result.Error == "" {
				t.Fatal(result)
			}
			if len(remote.in) != 0 {
				t.Fatal("unauthenticated test reached TUN")
			}
		})
	}
}
func TestVPNProbeCancellationAndNoLease(t *testing.T) {
	server, cfg, _ := fixture(t, 1)
	result := Probe(context.Background(), cfg)
	if !result.OK || result.AddressInUse {
		t.Fatal(result)
	}
	server.mu.Lock()
	n := len(server.peers)
	server.mu.Unlock()
	if n != 0 || server.cfg.Stats.Snapshot().Active != 0 {
		t.Fatal("probe reserved a lease")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result = Probe(ctx, cfg)
	if result.OK {
		t.Fatal("cancelled test succeeded")
	}
}
func TestVPNProbeMACDomainDoesNotAlterNormalProtocol(t *testing.T) {
	key := make([]byte, 32)
	h := hello{ID: "id", IP: "10.77.0.2", Network: "10.77.0.0/24", MTU: 1280}
	b, _ := json.Marshal([]any{Protocol, "challenge", h.ID, h.IP, h.Network, h.MTU})
	m := hmac.New(sha256.New, key)
	m.Write(b)
	normal := proof(key, "challenge", h)
	if normal != hex.EncodeToString(m.Sum(nil)) {
		t.Fatal("legacy authentication changed")
	}
	h.Probe = true
	if proof(key, "challenge", h) == normal {
		t.Fatal("old servers could allocate a test session")
	}
	w := welcome{Probe: true, IP: h.IP, Network: h.Network, MTU: h.MTU, Gateway: "10.77.0.1"}
	signed := welcomeProof(key, "challenge", h, w)
	w.AddressInUse = true
	if signed == welcomeProof(key, "challenge", h, w) {
		t.Fatal("availability not authenticated")
	}
}
