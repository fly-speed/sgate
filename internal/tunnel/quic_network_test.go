package tunnel

import (
	"bytes"
	"context"
	"crypto/rand"
	"net"
	"net/url"
	"sync"
	"testing"
	"time"
)

func TestQUICNetworkProfiles(t *testing.T) {
	for _, test := range []struct {
		name    string
		profile impairmentProfile
		rebind  bool
	}{
		{"reorder", impairmentProfile{Delay: time.Millisecond, ReorderEvery: 7, ReorderDelay: 8 * time.Millisecond}, false},
		{"rate", impairmentProfile{BytesPerSecond: 256 << 10}, false},
		{"mtu1250", impairmentProfile{Delay: time.Millisecond, MaxDatagram: 1250}, false},
		{"loss", impairmentProfile{Delay: 2 * time.Millisecond, DropEvery: 31}, false},
		{"nat-rebind", impairmentProfile{Delay: time.Millisecond}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t, time.Minute)
			opts := testOptions()
			opts.HeartbeatTimeout = 5 * time.Second
			opts.Stats = NewStats()
			endpoint, trust := nativeEndpoint(t, f.server)
			u, _ := url.Parse(endpoint)
			relay := newUDPProfile(t, u.Host, test.profile)
			u.Host = relay.listener.LocalAddr().String()
			client, err := NewClient(ClientConfig{Key: f.server.cfg.Key, Password: testPassword, URL: u.String(), TLSConfig: trust, Options: opts})
			if err != nil {
				t.Fatal(err)
			}
			local, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- client.Run(ctx, local) }()
			defer func() { cancel(); <-done }()
			conn, err := net.Dial("tcp", local.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(20 * time.Second))
			payload := make([]byte, 256<<10)
			if _, err := rand.Read(payload); err != nil {
				t.Fatal(err)
			}
			before := f.accepted.Load()
			start := time.Now()
			if err := exchange(conn, payload); err != nil {
				t.Fatal(err)
			}
			if test.rebind {
				if err := relay.rebind(); err != nil {
					t.Fatal(err)
				}
				if err := exchange(conn, payload); err != nil {
					t.Fatal("NAT change broke business socket", err)
				}
			}
			if test.name == "rate" && time.Since(start) < 700*time.Millisecond {
				t.Fatal("rate limit was not exercised")
			}
			if test.name == "reorder" && relay.reordered.Load() == 0 {
				t.Fatal("packets were not reordered")
			}
			if test.name == "mtu1250" && relay.mtuDrops.Load() == 0 {
				t.Fatal("path MTU drops were not exercised")
			}
			if test.name == "loss" && (relay.drops.Load() == 0 || opts.Stats.Snapshot().QUIC.RetransmittedStreamBytes == 0) {
				t.Fatal("network retransmission was not observed")
			}
			if f.accepted.Load() != before+1 {
				t.Fatal("network impairment replaced backend TCP socket")
			}
		})
	}
}
func TestQUICDistinctNativeQueueNegotiation(t *testing.T) {
	client, server := testOptions(), testOptions()
	client.NativeStreamBuffer = 32 << 10
	client.ReplayBytes = 128 << 10
	client.ReceiveBytes = 256 << 10
	server.NativeStreamBuffer = 64 << 10
	server.ReplayBytes = 256 << 10
	server.ReceiveBytes = 128 << 10
	f := newFixtureWithTransport(t, time.Minute, nil, client, server, true, true)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		c := f.dial(t)
		wg.Add(1)
		go func(c net.Conn) {
			defer wg.Done()
			if err := exchange(c, bytes.Repeat([]byte("independent windows"), 8192)); err != nil {
				t.Error(err)
			}
		}(c)
	}
	wg.Wait()
	f.server.mu.Lock()
	defer f.server.mu.Unlock()
	for _, entry := range f.server.sessions {
		if entry.session.native != nil {
			entry.session.mu.Lock()
			limit := entry.session.native.active.peerLimit
			entry.session.mu.Unlock()
			if limit != 32<<10 {
				t.Fatalf("peer flow window=%d", limit)
			}
		}
	}
}

func TestQUICFlowAdmissionPreservesExistingStreams(t *testing.T) {
	client, server := testOptions(), testOptions()
	client.MaxStreams = 4
	server.MaxStreams = 2
	stats := NewStats()
	server.Stats = stats
	f := newFixtureWithTransport(t, time.Minute, nil, client, server, true, true)
	a, b := f.dial(t), f.dial(t)
	if err := exchange(a, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := exchange(b, []byte("second")); err != nil {
		t.Fatal(err)
	}
	rejected := f.dial(t)
	rejected.SetDeadline(time.Now().Add(2 * time.Second))
	rejected.Write([]byte("over limit"))
	var p [1]byte
	if _, err := rejected.Read(p[:]); err == nil {
		t.Fatal("excess flow accepted")
	} else if e, ok := err.(net.Error); ok && e.Timeout() {
		t.Fatal("flow quota caused endless transport reconnects")
	}
	if err := exchange(a, []byte("first still healthy")); err != nil {
		t.Fatal(err)
	}
	if err := exchange(b, []byte("second still healthy")); err != nil {
		t.Fatal(err)
	}
	if stats.Snapshot().NoiseActivity.Disconnects != 0 {
		t.Fatal("flow admission reset unrelated transport")
	}
}
