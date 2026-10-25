package tunnel

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLiveSettingsAtomicValidationAndBroadcast(t *testing.T) {
	s, err := NewLiveSettings(LiveConfig{})
	if err != nil {
		t.Fatal(err)
	}
	initial, changed := s.Snapshot()
	if initial.NoiseMinBytes != 1024 || initial.ReconnectMin != 250*time.Millisecond {
		t.Fatal(initial)
	}
	invalid := initial
	invalid.NoiseMaxBytes = 1
	if s.Update(invalid) == nil {
		t.Fatal("invalid range accepted")
	}
	select {
	case <-changed:
		t.Fatal("invalid update notified writers")
	default:
	}
	next := initial
	next.NoiseIntervalMin = time.Millisecond
	next.NoiseIntervalMax = 3 * time.Millisecond
	if err := s.Update(next); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	default:
		t.Fatal("writer was not woken")
	}
	got, fresh := s.Snapshot()
	if got != next {
		t.Fatal("mixed snapshot", got)
	}
	if err := s.Update(next); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fresh:
		t.Fatal("unchanged config woke writer")
	default:
	}
}

// 用真实失败握手观察重试次数，区分噪音广播与退避变更的唤醒行为。
func TestLiveReconnectWaitIgnoresNoiseAndUsesNewBounds(t *testing.T) {
	var attempts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	live, err := NewLiveSettings(LiveConfig{ReconnectMin: 10 * time.Second, ReconnectMax: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(ClientConfig{
		Key: bytes.Repeat([]byte{17}, 32), Password: "live-reconnect-test-password",
		URL:     "ws" + strings.TrimPrefix(server.URL, "http"),
		Options: Options{Live: live, Logf: func(string, ...any) {}},
	})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { finished <- client.Run(ctx, listener) }()
	defer func() {
		cancel()
		select {
		case err := <-finished:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("reconnect timer prevented shutdown")
		}
	}()
	deadline := time.Now().Add(3 * time.Second)
	for attempts.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if attempts.Load() != 1 {
		t.Fatal("initial handshake was not attempted")
	}
	for index := 0; index < 20; index++ {
		next, _ := live.Snapshot()
		next.NoiseInterval = time.Duration(index+1) * time.Millisecond
		if err := live.Update(next); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if attempts.Load() != 1 {
		t.Fatal("noise changes restarted reconnect wait", attempts.Load())
	}
	next, _ := live.Snapshot()
	next.ReconnectMin, next.ReconnectMax = 10*time.Millisecond, 20*time.Millisecond
	if err := live.Update(next); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for attempts.Load() < 4 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if attempts.Load() < 4 {
		t.Fatal("new backoff did not wake and reschedule retries", attempts.Load())
	}
}
