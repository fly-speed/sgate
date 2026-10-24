package tunnel

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestNoiseValidation(t *testing.T) {
	for _, opts := range []Options{
		{NoiseInterval: -1}, {NoiseMinBytes: -1},
		{NoiseMinBytes: 100, NoiseMaxBytes: 99}, {NoiseMaxBytes: chunkSize + 1},
		{NoiseIntervalMin: time.Second}, {NoiseIntervalMax: time.Second},
		{NoiseIntervalMin: -1, NoiseIntervalMax: time.Second},
		{NoiseIntervalMin: 3 * time.Second, NoiseIntervalMax: time.Second},
		{NoiseInterval: time.Second, NoiseIntervalMin: time.Second, NoiseIntervalMax: 3 * time.Second},
	} {
		if _, err := opts.defaults(); err == nil {
			t.Fatalf("accepted invalid options: %+v", opts)
		}
	}
	for _, f := range []frame{
		{op: opNoise}, {op: opNoise, seq: 1, data: []byte{1}},
		{op: opNoise, ack: 1, data: []byte{1}}, {op: opNoise, stream: 1, data: []byte{1}},
		{op: opNoise, data: make([]byte, chunkSize+1)},
	} {
		if _, err := decodeFrame(f.encode()); err == nil {
			t.Fatal("accepted malformed noise frame")
		}
	}
	for _, size := range []int{1, chunkSize} {
		if _, err := decodeFrame((frame{op: opNoise, data: make([]byte, size)}).encode()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNoiseRandomIntervals(t *testing.T) {
	for _, opts := range []Options{
		{}, {NoiseInterval: time.Second},
		{NoiseIntervalMin: time.Second, NoiseIntervalMax: 3 * time.Second},
		{NoiseIntervalMin: time.Second, NoiseIntervalMax: time.Second},
		{NoiseIntervalMin: 1, NoiseIntervalMax: time.Duration(1<<63 - 1)},
	} {
		o, err := opts.defaults()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := o.defaults(); err != nil {
			t.Fatal("defaults are not idempotent:", err)
		}
		min, max := opts.NoiseIntervalMin, opts.NoiseIntervalMax
		if min == 0 {
			min, max = opts.NoiseInterval, opts.NoiseInterval
		}
		seen := make(map[time.Duration]bool)
		for i := 0; i < 100; i++ {
			d, err := o.noiseDelay()
			if err != nil || d < min || d > max {
				t.Fatalf("delay=%s outside [%s,%s], err=%v", d, min, max, err)
			}
			seen[d] = true
		}
		if min != max && len(seen) < 2 {
			t.Fatal("interval did not change between sends")
		}
	}
}

// Inspect decrypted wire frames, independently of the application echo tests.
func TestNoiseWireSizesAndWriterShutdown(t *testing.T) {
	for _, bounds := range [][2]int{{1024, 10240}, {chunkSize, chunkSize}} {
		t.Run(strconv.Itoa(bounds[0])+"-"+strconv.Itoa(bounds[1]), func(t *testing.T) {
			connections := make(chan *websocket.Conn, 1)
			h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err == nil {
					connections <- c
				}
			}))
			defer h.Close()
			peer, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.URL, "http"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer peer.Close()
			writer := <-connections
			defer writer.Close()
			wireOpts := Options{NoiseInterval: 5 * time.Millisecond, NoiseMinBytes: bounds[0], NoiseMaxBytes: bounds[1]}
			if bounds[0] != bounds[1] {
				wireOpts.NoiseInterval = 0
				wireOpts.NoiseIntervalMin = 5 * time.Millisecond
				wireOpts.NoiseIntervalMax = 15 * time.Millisecond
			}
			opts, err := wireOpts.defaults()
			if err != nil {
				t.Fatal(err)
			}
			s := newSession(context.Background(), "", opts)
			defer s.close()
			key := bytes.Repeat([]byte{9}, 32)
			tx := newCodec(key, "session", "c", "s", false)
			rx := newCodec(key, "session", "c", "s", true)
			done := make(chan struct{})
			finished := make(chan error, 1)
			go func() { finished <- s.writeTransport(writer, tx, done, nil) }()
			defer func() {
				close(done)
				writer.Close()
				select {
				case <-finished:
				case <-time.After(time.Second):
					t.Error("noise writer did not stop")
				}
			}()
			peer.SetReadDeadline(time.Now().Add(3 * time.Second))
			sizes := make(map[int]bool)
			var previous []byte
			for count := 0; count < 16; {
				_, sealed, err := peer.ReadMessage()
				if err != nil {
					t.Fatal(err)
				}
				plain, err := rx.open(sealed)
				if err != nil {
					t.Fatal(err)
				}
				f, err := decodeFrame(plain)
				if err != nil {
					t.Fatal(err)
				}
				if f.op == opAck {
					continue
				}
				if f.op != opNoise || len(f.data) < bounds[0] || len(f.data) > bounds[1] {
					t.Fatalf("unexpected frame op=%d size=%d", f.op, len(f.data))
				}
				if bytes.Equal(previous, f.data) {
					t.Fatal("padding contents repeated")
				}
				previous = f.data
				sizes[len(f.data)] = true
				count++
			}
			if bounds[0] != bounds[1] && len(sizes) < 2 {
				t.Fatal("padding sizes did not vary")
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.next != 0 || s.received != 0 || s.pendingBytes != 0 || len(s.pending) != 0 || len(s.streams) != 0 {
				t.Error("padding modified reliable session state")
			}
		})
	}
}

func TestNoiseDiscardAndReconnect(t *testing.T) {
	for _, mode := range []string{"client", "server", "both", "random-both"} {
		t.Run(mode, func(t *testing.T) {
			clientOpts, serverOpts := testOptions(), testOptions()
			if mode != "server" {
				clientOpts.NoiseInterval = 2 * time.Millisecond
			}
			if mode != "client" {
				serverOpts.NoiseInterval = 2 * time.Millisecond
			}
			if mode == "random-both" {
				clientOpts.NoiseInterval, serverOpts.NoiseInterval = 0, 0
				clientOpts.NoiseIntervalMin, serverOpts.NoiseIntervalMin = time.Millisecond, time.Millisecond
				clientOpts.NoiseIntervalMax, serverOpts.NoiseIntervalMax = 5*time.Millisecond, 5*time.Millisecond
			}
			f := newFixtureWithOptions(t, time.Minute, nil, clientOpts, serverOpts)
			time.Sleep(30 * time.Millisecond)
			if f.accepted.Load() != 0 {
				t.Fatal("idle padding opened backend connection")
			}
			c := f.dial(t)
			payload := bytes.Repeat([]byte("binary\x00\xffpayload"), 20000)
			if err := exchange(c, payload); err != nil {
				t.Fatal(err)
			}
			f.disconnect()
			waitFor(t, "detachment", func() bool { return !f.isAttached() })
			finished := make(chan error, 1)
			go func() { finished <- exchange(c, payload) }()
			time.Sleep(30 * time.Millisecond)
			f.blocked.Store(false)
			select {
			case err := <-finished:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("recovery with padding stalled")
			}
			c.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
			var one [1]byte
			if n, err := c.Read(one[:]); n != 0 {
				t.Fatal("padding or duplicate data reached application")
			} else if e, ok := err.(net.Error); !ok || !e.Timeout() {
				t.Fatalf("application connection closed: %v", err)
			}
			if f.accepted.Load() != 1 || f.bytes.Load() != int64(2*len(payload)) {
				t.Fatal("backend connection replaced or bytes lost/duplicated")
			}
			c.Close()
			waitFor(t, "noise leaves no stream or replay buffer", func() bool {
				f.server.mu.Lock()
				defer f.server.mu.Unlock()
				for _, e := range f.server.sessions {
					e.session.mu.Lock()
					clean := len(e.session.streams) == 0 && e.session.pendingBytes == 0
					e.session.mu.Unlock()
					if !clean {
						return false
					}
				}
				return true
			})
		})
	}
}
