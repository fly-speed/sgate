package vpn

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"io"
	"net/http/httptest"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

type memoryDevice struct {
	out, in chan []byte
	closed  chan struct{}
	once    sync.Once
}

func newMemoryDevice() *memoryDevice {
	return &memoryDevice{out: make(chan []byte, 128), in: make(chan []byte, 128), closed: make(chan struct{})}
}
func (d *memoryDevice) Read(p []byte) (int, error) {
	select {
	case <-d.closed:
		return 0, io.EOF
	case packet := <-d.out:
		if len(packet) > len(p) {
			return 0, io.ErrShortBuffer
		}
		return copy(p, packet), nil
	}
}
func (d *memoryDevice) Write(p []byte) (int, error) {
	select {
	case <-d.closed:
		return 0, io.EOF
	case d.in <- append([]byte(nil), p...):
		return len(p), nil
	}
}
func (d *memoryDevice) Close() error { d.once.Do(func() { close(d.closed) }); return nil }
func ipPacket(src, dst string, n int) []byte {
	packet := make([]byte, n)
	rand.Read(packet)
	packet[0] = 0x45
	binary.BigEndian.PutUint16(packet[2:4], uint16(n))
	packet[8] = 64
	packet[9] = 17
	a, b := netip.MustParseAddr(src).As4(), netip.MustParseAddr(dst).As4()
	copy(packet[12:16], a[:])
	copy(packet[16:20], b[:])
	packet[10] = 0
	packet[11] = 0
	var sum uint32
	for i := 0; i < 20; i += 2 {
		sum += uint32(binary.BigEndian.Uint16(packet[i : i+2]))
	}
	for sum>>16 != 0 {
		sum = (sum & 65535) + (sum >> 16)
	}
	binary.BigEndian.PutUint16(packet[10:12], ^uint16(sum))
	return packet
}
func await(t *testing.T, label string, fn func() bool) {
	t.Helper()
	until := time.Now().Add(5 * time.Second)
	for time.Now().Before(until) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timeout:", label)
}
func receivePacket(t *testing.T, d *memoryDevice, want []byte) {
	t.Helper()
	select {
	case actual := <-d.in:
		if !bytes.Equal(actual, want) {
			t.Fatal("packet changed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("IP packet not delivered")
	}
}
func fixture(t *testing.T, maxClients int) (*Server, Config, *memoryDevice) {
	t.Helper()
	seed := httptest.NewTLSServer(nil)
	defer seed.Close()
	roots := x509.NewCertPool()
	roots.AddCert(seed.Certificate())
	dev := newMemoryDevice()
	cfg := Config{Address: "127.0.0.1:0", TLS: seed.TLS, Key: bytes.Repeat([]byte{23}, 32), Network: netip.MustParsePrefix("10.77.0.0/24"), MTU: 1280, MaxClients: maxClients, Stats: &Stats{}}
	server, err := ListenServer(cfg, dev)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		server.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("server did not stop")
		}
	})
	cfg.Address = server.Addr().String()
	cfg.ClientIP = netip.MustParseAddr("10.77.0.2")
	cfg.TLS = &tls.Config{RootCAs: roots, ServerName: "127.0.0.1"}
	cfg.Stats = &Stats{}
	cfg.RetryMin = 10 * time.Millisecond
	cfg.RetryMax = 20 * time.Millisecond
	return server, cfg, dev
}
func startClient(t *testing.T, c Config) (*memoryDevice, context.CancelFunc, <-chan error) {
	t.Helper()
	dev := newMemoryDevice()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunClient(ctx, c, dev) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("client did not stop")
		}
	})
	return dev, cancel, done
}
func TestVPNDatagramsDuplexReconnectAndCleanup(t *testing.T) {
	server, cfg, remote := fixture(t, 4)
	var setup, cleanup atomic.Int32
	cfg.Connected = func() (func(), error) { setup.Add(1); return func() { cleanup.Add(1) }, nil }
	t.Cleanup(func() {
		if cleanup.Load() != 1 {
			t.Error("route cleanup was not called exactly once", cleanup.Load())
		}
	})
	local, _, _ := startClient(t, cfg)
	await(t, "authenticated VPN", func() bool { return cfg.Stats.Snapshot().Active == 1 })
	for _, n := range []int{60, 1000, 1280} {
		outgoing := ipPacket("10.77.0.2", "8.8.8.8", n)
		local.out <- outgoing
		receivePacket(t, remote, outgoing)
		incoming := ipPacket("8.8.8.8", "10.77.0.2", n)
		remote.out <- incoming
		receivePacket(t, local, incoming)
	}
	server.mu.Lock()
	p := server.peers[cfg.ClientIP]
	server.mu.Unlock()
	p.conn.CloseWithError(0, "injected outage")
	await(t, "VPN reconnect", func() bool { return cfg.Stats.Snapshot().Connections >= 2 && cfg.Stats.Snapshot().Active == 1 })
	outgoing := ipPacket("10.77.0.2", "8.8.4.4", 1280)
	local.out <- outgoing
	receivePacket(t, remote, outgoing)
	if setup.Load() != 1 {
		t.Fatal("reconnect reconfigured routes", setup.Load())
	}

}
func TestVPNRejectsBadAuthenticationAndUntrustedCertificate(t *testing.T) {
	for _, kind := range []string{"key", "certificate"} {
		t.Run(kind, func(t *testing.T) {
			server, cfg, remote := fixture(t, 1)
			if kind == "key" {
				cfg.Key = bytes.Repeat([]byte{24}, 32)
			} else {
				cfg.TLS = &tls.Config{RootCAs: x509.NewCertPool(), ServerName: "127.0.0.1"}
			}
			local, _, _ := startClient(t, cfg)
			local.out <- ipPacket("10.77.0.2", "8.8.8.8", 60)
			if kind == "key" {
				await(t, "authentication rejection", func() bool { return server.cfg.Stats.Snapshot().AuthRejected > 0 })
			} else {
				time.Sleep(200 * time.Millisecond)
			}
			if cfg.Stats.Snapshot().Connections != 0 {
				t.Fatal("unauthenticated VPN connected")
			}
			select {
			case <-remote.in:
				t.Fatal("unauthenticated packet reached TUN")
			default:
			}
		})
	}
}
func rawClient(t *testing.T, c Config, ip string) (*quic.Conn, welcome) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	q, err := quic.DialAddr(ctx, c.Address, &tls.Config{RootCAs: c.TLS.RootCAs, ServerName: "127.0.0.1", NextProtos: []string{Protocol}}, quicConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.CloseWithError(0, "test ended") })
	stream, err := q.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stream.SetDeadline(time.Now().Add(5 * time.Second))
	stream.Write([]byte{1})
	var challenge string
	if err = readJSON(stream, &challenge); err != nil {
		t.Fatal(err)
	}
	id, _ := randomNonce()
	h := hello{ID: id, IP: ip, Network: c.Network.String(), MTU: c.MTU}
	h.Proof = proof(c.Key, challenge, h)
	if err = writeJSON(stream, h); err != nil {
		t.Fatal(err)
	}
	var reply welcome
	if err = readJSON(stream, &reply); err != nil {
		t.Fatal(err)
	}
	stream.SetDeadline(time.Time{})
	return q, reply
}
func TestVPNRejectsSpoofingPrivateTargetsAndAddressCollision(t *testing.T) {
	server, cfg, remote := fixture(t, 1)
	conn, w := rawClient(t, cfg, "10.77.0.2")
	if w.Error != "" {
		t.Fatal(w.Error)
	}
	_, collision := rawClient(t, cfg, "10.77.0.2")
	if collision.Error == "" {
		t.Fatal("duplicate client address admitted")
	}
	_, capacity := rawClient(t, cfg, "10.77.0.3")
	if capacity.Error == "" {
		t.Fatal("client capacity exceeded")
	}
	for i, p := range [][]byte{ipPacket("10.77.0.3", "8.8.8.8", 60), ipPacket("10.77.0.2", "192.168.1.1", 60), ipPacket("10.77.0.2", "10.77.0.3", 60), ipPacket("10.77.0.2", "127.0.0.1", 60)} {
		if err := fragments(p, uint64(i+1), conn.SendDatagram); err != nil {
			t.Fatal(err)
		}
	}
	await(t, "bad IP packets dropped", func() bool { return server.cfg.Stats.Snapshot().Drops >= 4 })
	select {
	case <-remote.in:
		t.Fatal("forged/private packet reached host")
	default:
	}
	good := ipPacket("10.77.0.2", "8.8.8.8", 60)
	fragments(good, 100, conn.SendDatagram)
	receivePacket(t, remote, good)
}
func TestVPNFragmentLossReorderAndBounds(t *testing.T) {
	a, b := ipPacket("10.77.0.2", "8.8.8.8", 1280), ipPacket("10.77.0.2", "8.8.4.4", 1280)
	collect := func(p []byte, id uint64) [][]byte {
		var out [][]byte
		fragments(p, id, func(d []byte) error { out = append(out, d); return nil })
		return out
	}
	first, second := collect(a, 1), collect(b, 2)
	r := reassembler{}
	now := time.Now()
	r.push(first[0], 1280, now) // The lost tail of packet 1 must not block packet 2.
	if p, err := r.push(second[1], 1280, now); err != nil || p != nil {
		t.Fatal(err)
	}
	actual, err := r.push(second[0], 1280, now)
	if err != nil || !bytes.Equal(actual, b) {
		t.Fatal("reordering or packet isolation failed", err)
	}
	for i := 3; i < 66; i++ {
		r.push(collect(a, uint64(i))[0], 1280, now)
	}
	if len(r.packets) > 64 {
		t.Fatal("unbounded reassembly")
	}
	if _, err = r.push(collect(a, 100)[0], 1280, now); err == nil {
		t.Fatal("reassembly limit bypassed")
	}
	if _, err = r.push(collect(a, 100)[0], 1280, now.Add(3*time.Second)); err != nil || len(r.packets) != 1 {
		t.Fatal("stale fragments retained", err)
	}
	var v any
	if readJSON(bytes.NewReader([]byte{255, 255, 255, 255}), &v) == nil {
		t.Fatal("unbounded control record")
	}
}
func TestVPNPacketValidation(t *testing.T) {
	p := ipPacket("10.77.0.2", "8.8.8.8", 60)
	if _, _, ok := ipv4(p, 1280); !ok {
		t.Fatal("valid IPv4 rejected")
	}
	for _, invalid := range [][]byte{nil, p[:19], append(append([]byte(nil), p...), 0)} {
		if _, _, ok := ipv4(invalid, 1280); ok {
			t.Fatal("invalid packet accepted")
		}
	}
	for _, ip := range []string{"10.77.0.0", "10.77.0.1", "10.77.0.255", "10.78.0.2"} {
		if validClientIP(netip.MustParsePrefix("10.77.0.0/24"), netip.MustParseAddr(ip)) {
			t.Fatal("reserved address admitted", ip)
		}
	}
}

func TestVPNNetworkFailureIsFatalAndNeverRunsBeforeAuthentication(t *testing.T) {
	_, cfg, _ := fixture(t, 2)
	var called atomic.Int32
	cfg.Connected = func() (func(), error) { called.Add(1); return nil, io.ErrClosedPipe }
	dev := newMemoryDevice()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := RunClient(ctx, cfg, dev); err == nil {
		t.Fatal("network setup failure swallowed")
	}
	if called.Load() != 1 {
		t.Fatal("privileged setup retried", called.Load())
	}
	select {
	case <-dev.closed:
	default:
		t.Fatal("device retained after failure")
	}
	cfg.Key = bytes.Repeat([]byte{99}, 32)
	called.Store(0)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel2()
	if err := RunClient(ctx2, cfg, newMemoryDevice()); err != nil {
		t.Fatal(err)
	}
	if called.Load() != 0 {
		t.Fatal("network was configured before authentication")
	}
}
func TestVPNRejectsForgedServerProof(t *testing.T) {
	seed := httptest.NewTLSServer(nil)
	defer seed.Close()
	roots := x509.NewCertPool()
	roots.AddCert(seed.Certificate())
	serverTLS := seed.TLS.Clone()
	serverTLS.NextProtos = []string{Protocol}
	listener, err := quic.ListenAddr("127.0.0.1:0", serverTLS, quicConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		conn, err := listener.Accept(ctx)
		if err != nil {
			return
		}
		defer conn.CloseWithError(0, "test finished")
		control, err := conn.AcceptStream(ctx)
		if err != nil {
			return
		}
		var b [1]byte
		io.ReadFull(control, b[:])
		nonce, _ := randomNonce()
		writeJSON(control, nonce)
		var h hello
		readJSON(control, &h)
		writeJSON(control, welcome{IP: h.IP, Gateway: "10.77.0.1", Network: h.Network, MTU: h.MTU, Proof: "forged"})
		control.Read(b[:])
	}()
	var setup atomic.Int32
	cfg, err := (Config{Address: listener.Addr().String(), TLS: &tls.Config{RootCAs: roots, ServerName: "127.0.0.1"}, Key: bytes.Repeat([]byte{23}, 32), Network: netip.MustParsePrefix("10.77.0.0/24"), ClientIP: netip.MustParseAddr("10.77.0.2"), Connected: func() (func(), error) { setup.Add(1); return func() {}, nil }}).normalized(false)
	if err != nil {
		t.Fatal(err)
	}
	var active atomic.Pointer[peer]
	var cleanup func()
	dev := newMemoryDevice()
	defer dev.Close()
	id, _ := randomNonce()
	if err = clientConnection(ctx, cfg, id, &active, dev, &cleanup); err == nil {
		t.Fatal("forged server accepted")
	}
	if setup.Load() != 0 || cleanup != nil {
		t.Fatal("forged server changed client network")
	}
	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatal("forged-server worker did not stop")
	}
}
