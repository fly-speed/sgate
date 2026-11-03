package tunnel

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"github.com/quic-go/quic-go"
	"io"
	"net/http/httptest"
	"testing"
	"time"
)

// Exercise independent policies on real endpoints; QUIC uses a paced payload
// rate while WS retains its burst scheduler.
func TestTransportNoisePolicyParity(t *testing.T) {
	for _, mode := range []string{"websocket", "quic-legacy", "quic-native"} {
		for _, random := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/random=%t", mode, random), func(t *testing.T) {
				values := [2]LiveConfig{}
				lives := [2]*LiveSettings{}
				stats := [2]*Stats{NewStats(), NewStats()}
				opts := [2]Options{testOptions(), testOptions()}
				for i := range values {
					values[i] = LiveConfig{NoiseInterval: 20 * time.Millisecond, NoiseMinBytes: 2*chunkSize + 17, NoiseMaxBytes: 2*chunkSize + 17, NoiseSuppressTraffic: true, NoiseIdleAfter: 300 * time.Millisecond}
					if random {
						values[i].NoiseInterval = 0
						values[i].NoiseIntervalMin = 15 * time.Millisecond
						values[i].NoiseIntervalMax = 25 * time.Millisecond
					}
					var err error
					lives[i], err = NewLiveSettings(values[i])
					if err != nil {
						t.Fatal(err)
					}
					opts[i].Live = lives[i]
					opts[i].Stats = stats[i]
				}
				f := newFixtureWithTransport(t, time.Minute, nil, opts[0], opts[1], mode != "websocket", mode == "quic-native")
				update := func(i int) {
					t.Helper()
					if err := lives[i].Update(values[i]); err != nil {
						t.Fatal(err)
					}
				}
				tx := func(i int) uint64 { return stats[i].Snapshot().NoiseTX }
				progressed := func(i int, label string) {
					t.Helper()
					before := tx(i)
					waitFor(t, label, func() bool { return tx(i) >= before+uint64((2*chunkSize+17)/10) })
				}
				stopped := func(label string) {
					t.Helper()
					time.Sleep(50 * time.Millisecond)
					before := [2]uint64{tx(0), tx(1)}
					time.Sleep(80 * time.Millisecond)
					if tx(0) != before[0] || tx(1) != before[1] {
						t.Fatalf("%s emitted noise: %v -> [%d %d]", label, before, tx(0), tx(1))
					}
				}
				progressed(0, "client empty-session noise")
				progressed(1, "server empty-session noise")
				before := [2]uint64{tx(0), tx(1)}
				// More frequent unrelated updates than the noise interval must not starve it.
				until := time.Now().Add(200 * time.Millisecond)
				for iteration := 0; time.Now().Before(until); iteration++ {
					for i := range values {
						values[i].ReconnectMax = time.Duration(1+iteration%2) * time.Second
						update(i)
					}
					time.Sleep(4 * time.Millisecond)
				}
				for i := range stats {
					if tx(i) <= before[i]+uint64((2*chunkSize+17)/20) {
						t.Fatal("unrelated saves starved noise", mode, i)
					}
				}
				for i := range values {
					values[i].NoiseSuppressEmpty = true
					update(i)
				}
				stopped("empty disabled")
				conn := f.dial(t)
				if err := exchange(conn, []byte("business starts")); err != nil {
					t.Fatal(err)
				}
				stopped("business traffic disabled")
				progressed(0, "client idle-session noise")
				progressed(1, "server idle-session noise")
				for i := range values {
					values[i].NoiseSuppressIdle = true
					update(i)
				}
				stopped("all scenes disabled")
				serverBefore := stats[1].Snapshot()
				clientBefore := stats[0].Snapshot()
				values[0].NoiseSuppressIdle = false
				update(0)
				progressed(0, "client independently enabled")
				waitFor(t, "server received client noise", func() bool { return stats[1].Snapshot().NoiseRX > serverBefore.NoiseRX })
				if tx(1) != serverBefore.NoiseTX || stats[0].Snapshot().NoiseRX != clientBefore.NoiseRX {
					t.Fatal("one side's send setting changed the other side")
				}
				for i := range values {
					values[i].NoiseSuppressIdle = true
					values[i].NoiseSuppressTraffic = false
					update(i)
				}
				if err := exchange(conn, []byte("busy noise enabled")); err != nil {
					t.Fatal(err)
				}
				progressed(0, "client busy noise")
				progressed(1, "server busy noise")
				for i := range values {
					values[i].NoiseSuppressTraffic = true
					update(i)
				}
				stopped("live policy stops noise")
				a, b := stats[0].Snapshot(), stats[1].Snapshot()
				if a.NoiseTX != b.NoiseRX || b.NoiseTX != a.NoiseRX {
					t.Fatalf("bidirectional payload accounting differs: client %d/%d server %d/%d", a.NoiseTX, a.NoiseRX, b.NoiseTX, b.NoiseRX)
				}
				if a.NoiseActivity.Disconnects != 0 || b.NoiseActivity.Disconnects != 0 {
					t.Fatal("live noise policy reconnected business transport")
				}
			})
		}
	}
}

// Inspect decrypted native control records in both cryptographic directions,
// rather than treating different ciphertext as proof of random padding.
func TestNativeQUICNoiseRandomPayloadOnWire(t *testing.T) {
	for _, clientWriter := range []bool{false, true} {
		t.Run(fmt.Sprintf("client-writer=%t", clientWriter), func(t *testing.T) {
			seed := httptest.NewTLSServer(nil)
			defer seed.Close()
			roots := x509.NewCertPool()
			roots.AddCert(seed.Certificate())
			opts := testOptions()
			opts.nativeQUIC = true
			opts.Stats = NewStats()
			opts.NoiseInterval = 10 * time.Millisecond
			opts.NoiseMinBytes = 2*chunkSize + 17
			opts.NoiseMaxBytes = opts.NoiseMinBytes
			var err error
			opts, err = opts.defaults()
			if err != nil {
				t.Fatal(err)
			}
			serverTLS := seed.TLS.Clone()
			serverTLS.NextProtos = []string{nativeProtocol}
			listener, err := quic.ListenAddr("127.0.0.1:0", serverTLS, quicConfig(opts))
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client, err := quic.DialAddr(ctx, listener.Addr().String(), &tls.Config{RootCAs: roots, NextProtos: []string{nativeProtocol}}, quicConfig(opts))
			if err != nil {
				t.Fatal(err)
			}
			defer client.CloseWithError(0, "test complete")
			server, err := listener.Accept(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer server.CloseWithError(0, "test complete")
			clientStream, err := client.OpenStreamSync(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = clientStream.Write([]byte{0}); err != nil {
				t.Fatal(err)
			}
			serverStream, err := server.AcceptStream(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var marker [1]byte
			if _, err = io.ReadFull(serverStream, marker[:]); err != nil {
				t.Fatal(err)
			}
			writer, control, peer := server, serverStream, clientStream
			if clientWriter {
				writer, control, peer = client, clientStream, serverStream
			}
			key := bytes.Repeat([]byte{8}, 32)
			id, nonce, serverNonce := randomID(), randomID(), randomID()
			s := newSession(ctx, "", opts)
			nt := newNativeTransport(writer, control, newCodec(key, id, nonce, serverNonce, clientWriter), key, id, nonce, serverNonce)
			nt.s = s
			nt.traffic = newNativeTraffic(opts.Stats, writer.RemoteAddr())
			done := make(chan error, 1)
			go func() { done <- nt.writeControls() }()
			defer func() { nt.stop(); s.close(); <-done; nt.traffic.close() }()
			crypt := newCodec(key, id, nonce, serverNonce, !clientWriter)
			peer.SetReadDeadline(time.Now().Add(3 * time.Second))
			seen := map[string]bool{}
			for blocks := 0; blocks < 6; {
				sealed, err := readNativeRecord(peer, 3*chunkSize)
				if err != nil {
					t.Fatal(err)
				}
				plain, err := crypt.open(sealed)
				if err != nil {
					t.Fatal(err)
				}
				var control nativeControl
				if err = json.Unmarshal(plain, &control); err != nil {
					t.Fatal(err)
				}
				if control.Kind != "noise" {
					continue
				}
				expected := opts.NoiseMinBytes / 50
				if len(control.Data) != expected {
					t.Fatalf("burst chunk %d: %d != %d", blocks, len(control.Data), expected)
				}
				if bytes.Count(control.Data, control.Data[:1]) == len(control.Data) || seen[string(control.Data)] {
					t.Fatal("fixed or reused random padding")
				}
				seen[string(control.Data)] = true
				blocks++
			}
			waitFor(t, "noise payload statistics updated", func() bool { return opts.Stats.Snapshot().NoiseTX >= uint64(6*(opts.NoiseMinBytes/50)) })
		})
	}
}
