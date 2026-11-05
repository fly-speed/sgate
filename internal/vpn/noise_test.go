package vpn

import (
	"bytes"
	"context"
	"errors"
	"github.com/quic-go/quic-go"
	"io"
	"strings"
	"testing"
	"time"
)

func TestVPNNoiseDuplexDoesNotReachTUNOrCountAsIP(t *testing.T) {
	server, cfg, remote := fixtureNoise(t, 4, NoiseConfig{Rate: 24 * 1024, Traffic: true, Idle: true})
	cfg.Noise = NoiseConfig{Rate: 12 * 1024, Traffic: true, Idle: true}
	started := time.Now()
	local, cancel, _ := startClient(t, cfg)
	await(t, "bidirectional padding", func() bool { return cfg.Stats.Snapshot().NoiseTX > 5000 && cfg.Stats.Snapshot().NoiseRX > 5000 })
	if len(local.in) != 0 || len(remote.in) != 0 || cfg.Stats.Snapshot().TX != 0 || cfg.Stats.Snapshot().RX != 0 || cfg.Stats.Snapshot().Drops != 0 {
		t.Fatal("padding reached IP path", cfg.Stats.Snapshot())
	}
	server.mu.Lock()
	p := server.peers[cfg.ClientIP]
	server.mu.Unlock()
	if p == nil || !p.padding || p.flow == nil {
		t.Fatal("padding not negotiated")
	}
	if float64(cfg.Stats.Snapshot().NoiseTX) > float64(cfg.Noise.Rate)*time.Since(started).Seconds()+1000 {
		t.Fatal("padding exceeded target")
	}
	packet := ipPacket("10.77.0.2", "8.8.8.8", 1280)
	local.out <- packet
	receivePacket(t, remote, packet)
	reverse := ipPacket("8.8.8.8", "10.77.0.2", 1280)
	remote.out <- reverse
	receivePacket(t, local, reverse)
	cancel()
	await(t, "padding client stopped", func() bool { return cfg.Stats.Snapshot().Active == 0 })
	before := cfg.Stats.Snapshot().NoiseTX
	time.Sleep(30 * time.Millisecond)
	if cfg.Stats.Snapshot().NoiseTX != before {
		t.Fatal("padding continued offline")
	}
}
func TestVPNNoisePacingDropsDebtAndSupportsSmallRates(t *testing.T) {
	base := time.Unix(0, 0)
	p := noisePacer{last: base}
	if n := p.budget(base.Add(3*time.Second), 200000, true); n != 1000 {
		t.Fatal("catch-up burst", n)
	}
	if n := p.budget(base.Add(4*time.Second), 200000, false); n != 0 {
		t.Fatal(n)
	}
	if n := p.budget(base.Add(time.Hour), 200000, true); n != 1000 {
		t.Fatal("paused credit accumulated", n)
	}
	p = noisePacer{last: base}
	total := 0
	for i := 1; i <= 200; i++ {
		total += p.budget(base.Add(time.Duration(i)*noiseTick), 1, true)
	}
	if total != 1 {
		t.Fatal("fractional credit lost", total)
	}
}
func TestVPNNoisePolicyAndCongestionAdmission(t *testing.T) {
	base := time.Now()
	p := &peer{started: base}
	policy := NoiseConfig{Rate: 1000, Traffic: false, Idle: true, IdleAfter: time.Second}
	if !p.noiseAllowed(base, policy) {
		t.Fatal("initial connected idle was suppressed")
	}
	p.lastBusiness.Store(1)
	if p.noiseAllowed(base.Add(100*time.Millisecond), policy) || !p.noiseAllowed(base.Add(2*time.Second), policy) {
		t.Fatal("business/idle policy ignored")
	}
	flow := &datagramFlow{}
	flow.queued.Store(4)
	if flow.allowNoise() {
		t.Fatal("noise can fill QUIC queue")
	}
	flow.sent.Store(1)
	if !flow.allowNoise() {
		t.Fatal("send progress not observed")
	}
	flow.cwnd.Store(12000)
	flow.inflight.Store(11000)
	if flow.allowNoise() {
		t.Fatal("noise ignores congestion window")
	}
}
func TestVPNNoiseLocalPressureHoldAndRecovery(t *testing.T) {
	now := time.Now()
	p := &peer{started: now, queue: newPacketQueue(0, nil)}
	if !p.noiseCanSend(now) {
		t.Fatal("idle connection should allow noise")
	}
	p.noteNoisePressure(now)
	p.noteNoisePressure(now.Add(500 * time.Millisecond))
	p.noteNoisePressure(now.Add(100 * time.Millisecond))
	if p.noiseCanSend(now.Add(time.Second)) {
		t.Fatal("newer pressure must extend the hold")
	}
	if !p.noiseCanSend(now.Add(1500 * time.Millisecond)) {
		t.Fatal("noise did not resume after pressure cleared")
	}
	p.flow = newDatagramFlow()
	p.noteNoisePressure(now.Add(2 * time.Second))
	if !p.noiseCanSend(now.Add(2 * time.Second)) {
		t.Fatal("local heuristic overrode QUIC admission")
	}
}

func TestVPNNoiseYieldsToBulkOnNonQUICTransports(t *testing.T) {
	for _, transport := range []string{"udp", "tcp", "wss"} {
		t.Run(transport, func(t *testing.T) {
			t.Parallel()
			noise := NoiseConfig{Rate: 4096, Traffic: true, Idle: true}
			server, cfg, remote := fixtureWire(t, 4, noise, transport)
			cfg.Noise = noise
			local, _, _ := startClient(t, cfg)
			counters := func() [2]uint64 {
				return [2]uint64{cfg.Stats.Snapshot().NoiseTX, server.cfg.Stats.Snapshot().NoiseTX}
			}
			await(t, "idle padding on both ends", func() bool {
				rates := counters()
				return rates[0] > 0 && rates[1] > 0
			})
			packet := ipPacket("8.8.8.8", cfg.ClientIP.String(), 1280)
			remote.out <- packet
			receivePacket(t, local, packet)
			// A padding write already in progress may finish after delivery.
			time.Sleep(50 * time.Millisecond)
			paused := counters()
			time.Sleep(100 * time.Millisecond)
			if got := counters(); got != paused {
				t.Fatalf("padding continued during bulk pressure hold: %v → %v", paused, got)
			}
			await(t, "padding resumed on both ends", func() bool {
				rates := counters()
				return rates[0] > paused[0] && rates[1] > paused[1]
			})
		})
	}
}

func TestVPNNoiseLegacyPeerKeepsBusinessConnection(t *testing.T) {
	server, cfg, remote := fixtureNoise(t, 4, NoiseConfig{Rate: 24 * 1024, Traffic: true, Idle: true})
	cfg, err := cfg.normalized(false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := quic.DialAddr(ctx, cfg.Address, cfg.TLS, quicConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "done")
	control, err := conn.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	control.SetDeadline(time.Now().Add(3 * time.Second))
	control.Write([]byte{1})
	var challenge string
	if err = readJSON(control, &challenge); err != nil {
		t.Fatal(err)
	}
	id, _ := randomNonce()
	h := hello{ID: id, IP: cfg.ClientIP.String(), Network: cfg.Network.String(), MTU: cfg.MTU}
	h.Proof = proof(cfg.Key, challenge, h)
	if err = writeJSON(control, h); err != nil {
		t.Fatal(err)
	}
	var reply welcome
	if err = readJSON(control, &reply); err != nil || reply.Padding {
		t.Fatal(reply, err)
	}
	await(t, "legacy unsupported counter", func() bool { return server.cfg.Stats.Snapshot().NoiseUnsupported == 1 })
	packet := ipPacket("10.77.0.2", "8.8.8.8", 1280)
	if err = fragments(packet, 1, conn.SendDatagram); err != nil {
		t.Fatal(err)
	}
	receivePacket(t, remote, packet)
	time.Sleep(60 * time.Millisecond)
	if server.cfg.Stats.Snapshot().NoiseTX != 0 {
		t.Fatal("sent padding to old client")
	}
}
func TestVPNNoiseMACIsBoundToNegotiatedSupport(t *testing.T) {
	key := make([]byte, 32)
	h := hello{Padding: true, ID: strings.Repeat("0", 64), IP: "10.77.0.2", Network: "10.77.0.0/24", MTU: 1280}
	a := proof(key, "challenge", h)
	h.Padding = false
	if a == proof(key, "challenge", h) {
		t.Fatal("padding flag not signed")
	}
	h.Padding = true
	w := welcome{Padding: true}
	a = welcomeProof(key, "challenge", h, w)
	w.Padding = false
	if a == welcomeProof(key, "challenge", h, w) {
		t.Fatal("padding response not signed")
	}
}

func TestVPNNoiseOneWayDoesNotEcho(t *testing.T) {
	server, cfg, remote := fixtureNoise(t, 4, NoiseConfig{Rate: 16 * 1024, Traffic: true, Idle: true})
	cfg.Noise.Rate = 0
	local, _, _ := startClient(t, cfg)
	await(t, "server padding received", func() bool { return cfg.Stats.Snapshot().NoiseRX > 256 })
	if cfg.Stats.Snapshot().NoiseTX != 0 || server.cfg.Stats.Snapshot().NoiseRX != 0 || len(local.in) != 0 || len(remote.in) != 0 {
		t.Fatal("padding echoed or reached TUN")
	}
}

func TestVPNNoiseClientFallsBackWithLegacyServer(t *testing.T) {
	modern, cfg, _ := fixture(t, 4)
	listener, err := quic.ListenAddr("127.0.0.1:0", modern.cfg.TLS, quicConfig())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	delivered := make(chan []byte, 1)
	go func() {
		finished <- func() error {
			conn, e := listener.Accept(ctx)
			if e != nil {
				return e
			}
			defer conn.CloseWithError(0, "done")
			control, e := conn.AcceptStream(ctx)
			if e != nil {
				return e
			}
			var preface [1]byte
			if _, e = io.ReadFull(control, preface[:]); e != nil {
				return e
			}
			challenge, _ := randomNonce()
			if e = writeJSON(control, challenge); e != nil {
				return e
			}
			var h hello
			if e = readJSON(control, &h); e != nil {
				return e
			}
			if h.Padding || h.Proof != proof(cfg.Key, challenge, h) {
				return errors.New("new client broke legacy authentication")
			}
			reply := welcome{IP: h.IP, Network: h.Network, Gateway: cfg.Network.Addr().Next().String(), MTU: h.MTU}
			reply.Proof = welcomeProof(cfg.Key, challenge, h, reply)
			if e = writeJSON(control, reply); e != nil {
				return e
			}
			r := reassembler{}
			for {
				data, e := conn.ReceiveDatagram(ctx)
				if e != nil {
					return e
				}
				packet, e := r.push(data, cfg.MTU, time.Now())
				if e != nil {
					return e
				}
				if packet != nil {
					delivered <- packet
					return nil
				}
			}
		}()
	}()
	t.Cleanup(func() {
		cancel()
		listener.Close()
		select {
		case e := <-finished:
			if e != nil {
				t.Error(e)
			}
		case <-time.After(3 * time.Second):
			t.Error("legacy server did not stop")
		}
	})
	cfg.Address = listener.Addr().String()
	cfg.Noise = NoiseConfig{Rate: 16 * 1024, Traffic: true, Idle: true}
	local, _, _ := startClient(t, cfg)
	await(t, "old server compatibility", func() bool { return cfg.Stats.Snapshot().NoiseUnsupported == 1 })
	time.Sleep(60 * time.Millisecond)
	if cfg.Stats.Snapshot().NoiseTX != 0 {
		t.Fatal("sent padding to old server")
	}
	packet := ipPacket("10.77.0.2", "8.8.8.8", 1280)
	local.out <- packet
	select {
	case actual := <-delivered:
		if !bytes.Equal(actual, packet) {
			t.Fatal("legacy IP data changed")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("legacy business packet not delivered")
	}
}
