package app

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"sgate/internal/tunnel"
)

func waitNoiseBytes(t *testing.T, stats *tunnel.Stats, minimum uint64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if stats.Snapshot().NoiseTX >= minimum {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("server noise did not reach client", stats.Snapshot())
}

func startNoiseClient(t *testing.T, address string) *tunnel.Stats {
	t.Helper()
	stats := tunnel.NewStats()
	client, err := tunnel.NewClient(tunnel.ClientConfig{
		Key: bytes.Repeat([]byte{39}, 32), Password: "app-test-password", URL: "ws://" + address + "/tunnel",
		ReconnectMin: 10 * time.Millisecond, ReconnectMax: 20 * time.Millisecond,
		Options: tunnel.Options{Stats: stats, Logf: func(string, ...any) {}},
	})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		listener.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("noise client did not stop")
		}
	})
	return stats
}

func TestServerNoiseIntervalsApplyToLiveTransport(t *testing.T) {
	c := newController(t, "server", "target = 127.0.0.1:1\nnoise-interval = 1h\nnoise-min-bytes = 1K\nnoise-max-bytes = 1K\n")
	address := c.worker.listener.Addr().String()
	setConfig(t, c, "listen", address)
	clientStats := startNoiseClient(t, address)
	time.Sleep(150 * time.Millisecond)
	setConfig(t, c, "noise-interval", "100ms")
	time.Sleep(150 * time.Millisecond)
	if c.stats.Snapshot().NoiseTX != 0 || c.current.noiseStatus()["interval"] != "1h0m0s" {
		t.Fatal("save-only unexpectedly applied")
	}
	if err := c.apply(); err != nil {
		t.Fatal(err)
	}
	waitNoiseBytes(t, c.stats, 2*1024)
	deadline := time.Now().Add(time.Second)
	for clientStats.Snapshot().NoiseRX < 2048 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if clientStats.Snapshot().NoiseRX < 2048 {
		t.Fatal("server noise was not received")
	}
	if c.current.noiseStatus()["interval"] != "100ms" {
		t.Fatal("incorrect running interval")
	}
	setConfig(t, c, "noise-interval", "500ms")
	if err := c.apply(); err != nil {
		t.Fatal(err)
	}
	before := c.stats.Snapshot().NoiseTX
	time.Sleep(200 * time.Millisecond)
	if c.stats.Snapshot().NoiseTX != before {
		t.Fatal("longer interval did not take effect")
	}
	waitNoiseBytes(t, c.stats, before+1024)
	setConfig(t, c, "noise-interval", "0")
	setConfig(t, c, "noise-interval-min", "150ms")
	setConfig(t, c, "noise-interval-max", "250ms")
	if err := c.apply(); err != nil {
		t.Fatal(err)
	}
	before = c.stats.Snapshot().NoiseTX
	waitNoiseBytes(t, c.stats, before+2048)
	if c.current.noiseStatus()["mode"] != "random" {
		t.Fatal("range not applied")
	}
	setConfig(t, c, "noise-interval-min", "0")
	setConfig(t, c, "noise-interval-max", "0")
	if err := c.apply(); err != nil {
		t.Fatal(err)
	}
	before = c.stats.Snapshot().NoiseTX
	time.Sleep(300 * time.Millisecond)
	if c.stats.Snapshot().NoiseTX != before || c.current.noiseStatus()["mode"] != "off" {
		t.Fatal("noise did not stop")
	}
}

func TestServerNoiseCommandLineIntervalStillOverridesFile(t *testing.T) {
	c := newController(t, "server", "target = 127.0.0.1:1\nnoise-interval = 1h\nnoise-max-bytes = 10M\n", "-noise-interval", "80ms", "-noise-max-bytes", "1K")
	address := c.worker.listener.Addr().String()
	setConfig(t, c, "listen", address)
	startNoiseClient(t, address)
	waitNoiseBytes(t, c.stats, 2048)
	setConfig(t, c, "noise-interval", "2h")
	if err := c.apply(); err != nil {
		t.Fatal(err)
	}
	before := c.stats.Snapshot().NoiseTX
	waitNoiseBytes(t, c.stats, before+2048)
	running := c.status().(map[string]any)["noise"].(map[string]any)
	if running["interval"] != "80ms" || running["max_bytes"] != 1024 {
		t.Fatal("command-line noise options lost", running)
	}
}
