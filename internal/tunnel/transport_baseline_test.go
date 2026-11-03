package tunnel

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Each case is run in a fresh process by scripts/benchmark-transports.py.
// Payloads are verified; no performance threshold is used as a correctness test.
func TestTransportBaseline(t *testing.T) {
	output := os.Getenv("SGATE_BASELINE_OUTPUT")
	if output == "" {
		t.Skip("use scripts/benchmark-transports.py")
	}
	mode := os.Getenv("SGATE_BASELINE_MODE")
	flows, _ := strconv.Atoi(os.Getenv("SGATE_BASELINE_FLOWS"))
	if flows <= 0 {
		flows = 8
	}
	size, _ := strconv.Atoi(os.Getenv("SGATE_BASELINE_BYTES"))
	if size <= 0 {
		size = 4 << 20
	}
	stats, serverStats := NewStats(), NewStats()
	opts := testOptions()
	opts.Stats = serverStats
	opts.BufferBytes = 4 << 20
	backend := targetBackend(t, "")
	server, err := NewServer(ServerConfig{Key: bytes.Repeat([]byte{8}, 32), Password: testPassword, Target: backend, Options: opts})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	web := httptest.NewTLSServer(server)
	defer web.Close()
	roots := x509.NewCertPool()
	roots.AddCert(web.Certificate())
	endpoint := "wss" + strings.TrimPrefix(web.URL, "https") + "/tunnel"
	if mode == "quic-v1" || mode == "quic-v2" {
		listener, err := ListenQUIC("127.0.0.1:0", web.TLS, opts, server)
		if err != nil {
			t.Fatal(err)
		}
		httpServer := &http.Server{Handler: server}
		go httpServer.Serve(listener)
		defer httpServer.Close()
		endpoint = "quic://" + listener.Addr().String() + "/tunnel"
	} else if mode != "wss" {
		t.Fatal("unknown baseline mode")
	}
	profile := os.Getenv("SGATE_BASELINE_PROFILE")
	if profile == "" {
		profile = "loopback"
	}
	var impairment *udpImpairment
	var shaping impairmentProfile
	if profile == "udp-loss" {
		shaping = impairmentProfile{Delay: 10 * time.Millisecond, DropEvery: 97}
	}
	if strings.HasPrefix(profile, "udp-matrix") || strings.HasPrefix(profile, "linux-netem") {
		if err := json.Unmarshal([]byte(os.Getenv("SGATE_BASELINE_IMPAIRMENT")), &shaping); err != nil {
			t.Fatal(err)
		}
	}
	if strings.HasPrefix(profile, "linux-netem") {
		u, _ := url.Parse(endpoint)
		configureNetem(t, u.Host, mode)
	} else if profile == "udp-loss" || strings.HasPrefix(profile, "udp-matrix") {
		if mode == "wss" {
			t.Fatal("UDP impairment only compares QUIC protocols")
		}
		u, _ := url.Parse(endpoint)
		impairment = newUDPProfile(t, u.Host, shaping)
		u.Host = impairment.listener.LocalAddr().String()
		endpoint = u.String()
	} else if profile != "loopback" {
		t.Fatal("unknown network profile")
	}
	opts.Stats = stats
	cfg := ClientConfig{Key: server.cfg.Key, Password: testPassword, URL: endpoint, TLSConfig: &tls.Config{RootCAs: roots}, Options: opts}
	if mode == "quic-v1" {
		cfg.QUICMode = "legacy"
	}
	client, err := NewClient(cfg)
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
	conns := make([]net.Conn, flows)
	for i := range conns {
		c, err := net.Dial("tcp", local.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(2 * time.Minute))
		conns[i] = c
		if err := exchange(c, []byte("warmup")); err != nil {
			t.Fatal(err)
		}
	}
	before := stats.Snapshot()
	cpu0, _ := baselineUsage()
	var mem0, mem1 runtime.MemStats
	runtime.ReadMemStats(&mem0)
	started := time.Now()
	var wg sync.WaitGroup
	payload := bytes.Repeat([]byte{0x69}, size)
	for _, c := range conns {
		wg.Add(1)
		go func(c net.Conn) {
			defer wg.Done()
			if err := exchange(c, payload); err != nil {
				t.Error(err)
			}
		}(c)
	}
	wg.Wait()
	elapsed := time.Since(started)
	cpu1, rss := baselineUsage()
	runtime.ReadMemStats(&mem1)
	after := stats.Snapshot()
	latencies := make([]float64, 0, flows*50)
	var mu sync.Mutex
	for _, c := range conns {
		wg.Add(1)
		go func(c net.Conn) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				start := time.Now()
				if err := exchange(c, []byte("ping")); err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				latencies = append(latencies, float64(time.Since(start))/1e6)
				mu.Unlock()
			}
		}(c)
	}
	wg.Wait()
	if t.Failed() {
		return
	}
	// A separate small flow measures latency while the existing flows transfer
	// continuously. All payloads, including the background load, are verified.
	ping, err := net.Dial("tcp", local.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer ping.Close()
	ping.SetDeadline(time.Now().Add(2 * time.Minute))
	if err := exchange(ping, []byte("mixed warmup")); err != nil {
		t.Fatal(err)
	}
	loadCtx, stopLoad := context.WithCancel(context.Background())
	defer stopLoad()
	loadPayload := bytes.Repeat([]byte("load"), 16<<10)
	var mixedBytes atomic.Uint64
	var loadWG sync.WaitGroup
	for _, c := range conns {
		loadWG.Add(1)
		go func(c net.Conn) {
			defer loadWG.Done()
			for loadCtx.Err() == nil {
				if err := exchange(c, loadPayload); err != nil {
					if loadCtx.Err() == nil {
						t.Error(err)
					}
					return
				}
				mixedBytes.Add(uint64(len(loadPayload) * 2))
			}
		}(c)
	}
	mixedLatencies := make([]float64, 0, 50)
	mixedTimeouts := 0
	mixedStarted := time.Now()
	for i := 0; i < 50; i++ {
		if time.Since(mixedStarted) > 5*time.Second {
			break
		}
		ping.SetDeadline(time.Now().Add(3 * time.Second))
		start := time.Now()
		if err := exchange(ping, []byte("ping")); err != nil {
			if e, ok := err.(net.Error); ok && e.Timeout() {
				mixedTimeouts++
			} else {
				t.Error(err)
			}
			break
		}
		mixedLatencies = append(mixedLatencies, float64(time.Since(start))/1e6)
	}
	stopLoad()
	for _, c := range conns {
		c.Close()
	}
	loadWG.Wait()
	if t.Failed() {
		return
	}
	sort.Float64s(mixedLatencies)
	sort.Float64s(latencies)
	result := map[string]any{"mode": mode, "flows": flows, "bytes_per_flow": size, "payload_verified": true, "elapsed_seconds": elapsed.Seconds(), "payload_mbps": float64(flows*size*2) * 8 / elapsed.Seconds() / 1e6, "cpu_seconds": cpu1 - cpu0, "process_peak_rss_bytes": rss, "allocated_bytes": mem1.TotalAlloc - mem0.TotalAlloc, "ping_p50_ms": baselineQuantile(latencies, 50), "ping_p95_ms": baselineQuantile(latencies, 95), "ping_p99_ms": baselineQuantile(latencies, 99), "transport_tx_bytes": after.TX - before.TX, "transport_rx_bytes": after.RX - before.RX, "quic_packet_tx_bytes": after.QUIC.PacketBytesTX - before.QUIC.PacketBytesTX, "quic_packet_rx_bytes": after.QUIC.PacketBytesRX - before.QUIC.PacketBytesRX, "quic_lost_packets": after.QUIC.LostPackets - before.QUIC.LostPackets, "quic_retransmitted_stream_bytes": after.QUIC.RetransmittedStreamBytes - before.QUIC.RetransmittedStreamBytes, "quic_blocked_frames": after.QUIC.ConnectionBlockedFrames + after.QUIC.StreamBlockedFrames - before.QUIC.ConnectionBlockedFrames - before.QUIC.StreamBlockedFrames, "quic": after.QUIC, "platform": runtime.GOOS + "/" + runtime.GOARCH, "go_version": runtime.Version(), "network_profile": profile, "cpu_scope": "client+server+echo+load generator in same process", "rss_scope": "process lifetime peak through bulk phase, including setup"}
	result["impairment"] = shaping
	result["cpu_rss_available"] = runtime.GOOS == "linux" || runtime.GOOS == "darwin"
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		result["cpu_seconds"], result["process_peak_rss_bytes"] = nil, nil
	}
	result["mixed_ping_timeouts"] = mixedTimeouts
	result["mixed_ping_completed"] = len(mixedLatencies)
	result["mixed_sampling_seconds"] = time.Since(mixedStarted).Seconds()
	result["mixed_sampling_complete"] = len(mixedLatencies) == 50
	result["mixed_ping_p95_ms"], result["mixed_ping_p99_ms"] = nil, nil
	if mixedTimeouts == 0 && len(mixedLatencies) == 50 {
		result["mixed_ping_p95_ms"] = baselineQuantile(mixedLatencies, 95)
		result["mixed_ping_p99_ms"] = baselineQuantile(mixedLatencies, 99)
	}
	result["mixed_verified_bytes"] = mixedBytes.Load()
	if impairment != nil {
		result["relay_dropped_datagrams"] = impairment.drops.Load()
		result["relay_delay_one_way_ms"] = float64(shaping.Delay) / float64(time.Millisecond)
		result["relay_drop_every_nth_datagram"] = shaping.DropEvery
		result["relay_reordered_datagrams"] = impairment.reordered.Load()
		result["relay_mtu_drops"] = impairment.mtuDrops.Load()
	}
	data, _ := json.MarshalIndent(result, "", "  ")
	if err := os.WriteFile(output, data, 0600); err != nil {
		t.Fatal(err)
	}
}

// Nearest-rank empirical quantile: with 50 observations P99 is the maximum,
// rather than silently reporting the second-largest observation as P99.
func baselineQuantile(sorted []float64, percent int) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := (len(sorted)*percent + 99) / 100
	return sorted[max(0, min(len(sorted)-1, rank-1))]
}
func TestBaselineQuantileCalibration(t *testing.T) {
	samples := make([]float64, 50)
	for i := range samples {
		samples[i] = float64(i + 1)
	}
	if p50, p95, p99 := baselineQuantile(samples, 50), baselineQuantile(samples, 95), baselineQuantile(samples, 99); p50 != 25 || p95 != 48 || p99 != 50 {
		t.Fatalf("incorrect empirical ranks: %v/%v/%v", p50, p95, p99)
	}
}
