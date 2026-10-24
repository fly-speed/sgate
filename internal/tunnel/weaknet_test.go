package tunnel

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"fmt"
	"github.com/gorilla/websocket"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type weakProfile struct {
	delay, jitter, spacing, retransmit time.Duration
	lossPercent                        int
}
type weakPacket struct {
	data []byte
	due  time.Time
}

// This relay models ordered TCP delivery. A lost synthetic segment waits for
// retransmission; arbitrary removal of TCP stream bytes is NOT packet loss.
// Queues are bounded, and a blackholed direction retains bytes until recovery
// or socket close, as a stalled TCP send window would.
type weakRelay struct {
	listener                                             net.Listener
	target                                               string
	profile                                              atomic.Pointer[weakProfile]
	blackhole                                            [2]atomic.Bool // 0: uplink; 1: downlink
	accepted, active, losses, queued, upBytes, downBytes atomic.Int64
	writeCalls, coalesced, waitTimers                    atomic.Int64
	ctx                                                  context.Context
	cancel                                               context.CancelFunc
	wg                                                   sync.WaitGroup
	mu                                                   sync.Mutex
	logf                                                 func(string, ...any)
	pairs                                                map[*weakPair]bool
}
type weakPair struct {
	a, b          net.Conn
	cancel        context.CancelFunc
	once          sync.Once
	trace         atomic.Bool
	tracked       atomic.Bool
	closeCause    atomic.Value
	read, written [2]atomic.Int64
}

func (p *weakPair) close() { p.closeWithReason("owner or pump exit") }
func (p *weakPair) closeWithReason(reason string) {
	p.once.Do(func() {
		p.closeCause.Store(time.Now().Format(time.RFC3339Nano) + " " + reason)
		p.cancel()
		p.a.Close()
		p.b.Close()
	})
}
func newWeakRelay(t *testing.T, target string) *weakRelay {
	return newWeakRelayDialTimeout(t, target, time.Second)
}

func newWeakRelayDialTimeout(t *testing.T, target string, dialTimeout time.Duration) *weakRelay {
	t.Helper()
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &weakRelay{listener: l, target: target, ctx: ctx, cancel: cancel, pairs: make(map[*weakPair]bool)}
	r.profile.Store(&weakProfile{})
	r.logf = t.Logf
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		for {
			a, e := l.Accept()
			if e != nil {
				return
			}
			b, e := (&net.Dialer{Timeout: dialTimeout}).DialContext(ctx, "tcp", target)
			if e != nil {
				a.Close()
				continue
			}
			pairCtx, pairCancel := context.WithCancel(ctx)
			p := &weakPair{a: a, b: b, cancel: pairCancel}
			r.mu.Lock()
			r.pairs[p] = true
			r.mu.Unlock()
			r.accepted.Add(1)
			r.active.Add(1)
			r.wg.Add(1)
			go func() {
				defer r.wg.Done()
				var pumps sync.WaitGroup
				for direction := 0; direction < 2; direction++ {
					src, dst := a, b
					if direction == 1 {
						src, dst = b, a
					}
					pumps.Add(1)
					go func(direction int, src, dst net.Conn) {
						defer pumps.Done()
						r.pump(pairCtx, p, direction, src, dst)
					}(direction, src, dst)
				}
				pumps.Wait()
				p.close()
				r.mu.Lock()
				delete(r.pairs, p)
				r.mu.Unlock()
				r.active.Add(-1)
			}()
		}
	}()
	t.Cleanup(func() {
		r.close()
		if r.active.Load() != 0 || r.queued.Load() != 0 {
			t.Errorf("weak relay leaked connections=%d bytes=%d", r.active.Load(), r.queued.Load())
		}
	})
	return r
}

// Idempotent shutdown also lets stress tests sample resources before T.Cleanup.
func (r *weakRelay) close() {
	r.cancel()
	r.listener.Close()
	r.mu.Lock()
	for p := range r.pairs {
		p.close()
	}
	r.mu.Unlock()
	r.wg.Wait()
}

// Adjacent segments whose individual delivery times have passed can share one
// TCP write. Never pass a future segment or cross an ordering barrier.
func dueWeakBatch(first weakPacket, queue <-chan weakPacket, now time.Time, limit int) ([]byte, *weakPacket, int) {
	data, merged := first.data, 0
	for len(data) < limit {
		select {
		case next, ok := <-queue:
			if !ok {
				return data, nil, merged
			}
			if next.due.After(now) || len(data)+len(next.data) > limit {
				return data, &next, merged
			}
			data = append(data, next.data...)
			merged++
		default:
			return data, nil, merged
		}
	}
	return data, nil, merged
}

func (r *weakRelay) pump(ctx context.Context, p *weakPair, direction int, src, dst net.Conn) {
	queue := make(chan weakPacket, 64) // at most 32 KiB per direction
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		defer close(queue)
		rng := rand.New(rand.NewPCG(uint64(direction+1), uint64(r.accepted.Load()+123)))
		buf := make([]byte, 4096)
		for {
			n, e := src.Read(buf)
			if p.tracked.Load() || p.trace.Load() {
				p.read[direction].Add(int64(n))
			}
			if e != nil {
				if p.trace.Load() {
					r.logf(
						"probe relay %s dir=%d read %s -> %s n=%d err=%v",
						time.Now().Format(time.RFC3339Nano),
						direction,
						src.RemoteAddr(),
						src.LocalAddr(),
						n,
						e,
					)
				}
				p.closeWithReason(fmt.Sprintf("read direction=%d: %v", direction, e))
				return
			}
			for offset := 0; offset < n; {
				end := min(offset+512, n)
				profile := r.profile.Load()
				delay := profile.delay
				if profile.jitter > 0 {
					delay += time.Duration(rng.Int64N(int64(profile.jitter)*2+1)) - profile.jitter
				}
				if delay < 0 {
					delay = 0
				}
				if profile.lossPercent > 0 && rng.IntN(100) < profile.lossPercent {
					delay += profile.retransmit
					r.losses.Add(1)
				}
				data := append([]byte(nil), buf[offset:end]...)
				packet := weakPacket{data: data, due: time.Now().Add(delay)}
				r.queued.Add(int64(len(data)))
				select {
				case queue <- packet:
				case <-ctx.Done():
					r.queued.Add(-int64(len(data)))
					return
				}
				offset = end
			}
		}
	}()
	var pending *weakPacket
	defer func() {
		p.close()
		<-readerDone
		if pending != nil {
			r.queued.Add(-int64(len(pending.data)))
		}
		for packet := range queue {
			r.queued.Add(-int64(len(packet.data)))
		}
	}()
	var lastWrite time.Time
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		var packet weakPacket
		if pending != nil {
			packet, pending = *pending, nil
		} else {
			select {
			case <-ctx.Done():
				return
			case next, ok := <-queue:
				if !ok {
					return
				}
				packet = next
			}
		}
		deliver := packet.due
		spacing := r.profile.Load().spacing
		if lastWrite.Add(spacing).After(deliver) {
			deliver = lastWrite.Add(spacing)
		}
		if delay := time.Until(deliver); delay > 0 {
			r.waitTimers.Add(1)
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				r.queued.Add(-int64(len(packet.data)))
				return
			case <-timer.C:
			}
		}
		for r.blackhole[direction].Load() {
			timer := time.NewTimer(5 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				r.queued.Add(-int64(len(packet.data)))
				return
			case <-timer.C:
			}
		}
		data := packet.data
		if spacing == 0 {
			var merged int
			data, pending, merged = dueWeakBatch(packet, queue, time.Now(), chunkSize)
			r.coalesced.Add(int64(merged))
		}
		r.writeCalls.Add(1)
		n, e := io.Copy(dst, bytes.NewReader(data))
		r.queued.Add(-int64(len(data)))
		if p.tracked.Load() || p.trace.Load() {
			p.written[direction].Add(n)
		}
		if e != nil {
			p.closeWithReason(fmt.Sprintf("write direction=%d: %v", direction, e))
			if p.trace.Load() {
				r.logf(
					"probe relay %s dir=%d write %s -> %s err=%v",
					time.Now().Format(time.RFC3339Nano),
					direction,
					dst.LocalAddr(),
					dst.RemoteAddr(),
					e,
				)
			}
			return
		}
		lastWrite = time.Now()
		if direction == 0 {
			r.upBytes.Add(int64(len(data)))
		} else {
			r.downBytes.Add(int64(len(data)))
		}
	}
}
func weakFixture(t *testing.T, options Options, ttl, offline time.Duration) (*recoveryFixture, *weakRelay) {
	t.Helper()
	return weakFixtureMode(t, options, ttl, offline, "")
}

func weakFixtureMode(t *testing.T, options Options, ttl, offline time.Duration, mode string) (
	*recoveryFixture,
	*weakRelay,
) {
	t.Helper()
	return weakFixtureBackend(t, options, ttl, offline, mode, nil)
}

func weakFixtureBackend(t *testing.T, options Options, ttl, offline time.Duration, mode string, backend func(net.Conn)) (
	*recoveryFixture,
	*weakRelay,
) {
	t.Helper()
	base := runtime.NumGoroutine()
	t.Cleanup(func() {
		waitFor(
			t,
			"weak relay and tunnel goroutines released",
			func() bool { return runtime.NumGoroutine() <= base+4 },
		)
	})
	var relay *weakRelay
	f := newRecoveryFixtureBackend(t, ttl, offline, mode, options, backend, func(c *ClientConfig) {
		u, e := url.Parse(c.URL)
		if e != nil {
			t.Fatal(e)
		}
		relay = newWeakRelay(t, u.Host)
		u.Host = relay.listener.Addr().String()
		c.URL = u.String()
	})
	if recoveryNoiseEnabled(options) {
		t.Cleanup(func() {
			client, remote := f.clientSession(), f.remoteSession()
			f.cancel()
			f.stopServer()
			for _, s := range []*session{client, remote} {
				if s != nil {
					// transportMu is released only after the writer returns and
					// its heartbeat ticker and padding timer have been stopped.
					assertReleased(t, "noisy weak-network shutdown", s)
				}
			}
			waitFor(t, "noisy relay sockets and queued bytes released", func() bool {
				return relay.active.Load() == 0 && relay.queued.Load() == 0
			})
		})
	}
	return f, relay
}

func TestWeakNetworkSlowFrameProgressDoesNotReconnect(t *testing.T) {
	testWeakNetworkSlowFrameProgressDoesNotReconnect(t, nil, "")
}

func testWeakNetworkSlowFrameProgressDoesNotReconnect(t *testing.T, configure func(*Options), mode string) {
	opts := Options{
		Heartbeat:    100 * time.Millisecond,
		DialTimeout:  200 * time.Millisecond,
		WriteTimeout: 200 * time.Millisecond,
		BufferBytes:  64 * 1024,
		Logf:         func(string, ...any) {},
	}
	if configure != nil {
		configure(&opts)
	}
	f, relay := weakFixtureMode(t, opts, 5*time.Second, 5*time.Second, mode)
	warmupRecoveryNoise(t, f)
	checkNoise := weakNoiseProgress(t, f)
	app := f.dial()
	if e := exchange(app, []byte("baseline")); e != nil {
		t.Fatal(e)
	}
	accepted := relay.accepted.Load()
	relay.profile.Store(&weakProfile{spacing: 20 * time.Millisecond})
	payload := make([]byte, chunkSize)
	_, _ = cryptorand.Read(payload)
	done := make(chan error, 1)
	go func() { done <- exchange(app, payload) }()
	time.Sleep(500 * time.Millisecond)
	if relay.accepted.Load() != accepted {
		t.Fatal("healthy partial-frame progress caused a heartbeat reconnect")
	}
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("slow frame transfer stalled")
	}
	if relay.accepted.Load() != accepted {
		t.Fatal("healthy slow connection reconnected")
	}
	if f.backendCount.accepted.Load() != 1 {
		t.Fatal("backend socket replaced")
	}
	checkNoise()
}

func finishWeakStream(t *testing.T, f *recoveryFixture, app net.Conn) {
	t.Helper()
	if e := app.(*net.TCPConn).CloseWrite(); e != nil {
		t.Fatal(e)
	}
	var b [1]byte
	if n, e := app.Read(b[:]); n != 0 || e != io.EOF {
		t.Fatalf("extra bytes or missing half-close: %d %v", n, e)
	}
	app.Close()
	waitFor(t, "weak-network streams and buffers released", func() bool {
		if f.localCount.active.Load() != 0 || f.backendCount.active.Load() != 0 {
			return false
		}
		client, remote := f.clientSession(), f.remoteSession()
		if remote == nil {
			return false
		}
		for _, s := range []*session{client, remote} {
			s.mu.Lock()
			clean := len(s.streams) == 0 && s.pending == nil && s.pendingBytes == 0
			s.mu.Unlock()
			if !clean {
				return false
			}
		}
		return true
	})
}

func TestWeakNetworkLatencyJitterAndRetransmission(t *testing.T) {
	testWeakNetworkLatencyJitterAndRetransmission(t, nil, "")
}

func testWeakNetworkLatencyJitterAndRetransmission(t *testing.T, configure func(*Options), mode string) {
	opts := Options{
		Heartbeat:        100 * time.Millisecond,
		HeartbeatTimeout: 2 * time.Second,
		DialTimeout:      time.Second,
		WriteTimeout:     time.Second,
		BufferBytes:      128 * 1024,
		Logf:             func(string, ...any) {},
	}
	if configure != nil {
		configure(&opts)
	}
	f, relay := weakFixtureMode(t, opts, 8*time.Second, 8*time.Second, mode)
	warmupRecoveryNoise(t, f)
	checkNoise := weakNoiseProgress(t, f)
	anchor := f.dial()
	if e := exchange(anchor, []byte("baseline")); e != nil {
		t.Fatal(e)
	}
	accepted := relay.accepted.Load()
	relay.profile.Store(&weakProfile{
		delay:       150 * time.Millisecond,
		jitter:      100 * time.Millisecond,
		lossPercent: 15,
		retransmit:  200 * time.Millisecond,
	})
	// Remain idle for longer than the receive timeout, so Ping/Pong must sustain the connection.
	time.Sleep(2200 * time.Millisecond)
	if relay.accepted.Load() != accepted {
		t.Fatal("latency/jitter falsely disconnected idle healthy transport")
	}
	var wg sync.WaitGroup
	errors := make(chan error, 4)
	for i := 0; i < 4; i++ {
		app := f.dial()
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer app.Close()
			p := make([]byte, 64*1024)
			_, _ = cryptorand.Read(p)
			if e := exchange(app, p); e != nil {
				errors <- e
				return
			}
			if e := app.(*net.TCPConn).CloseWrite(); e != nil {
				errors <- e
				return
			}
			var b [1]byte
			if n, e := app.Read(b[:]); n != 0 || e != io.EOF {
				errors <- fmt.Errorf("unexpected tail: %d %v", n, e)
			}
		}()
	}
	wg.Wait()
	close(errors)
	for e := range errors {
		t.Fatal(e)
	}
	if relay.accepted.Load() != accepted {
		t.Fatal("healthy delayed transfer caused reconnect")
	}
	if relay.losses.Load() == 0 {
		t.Fatal("no retransmission delay was injected")
	}
	if f.backendCount.accepted.Load() != 5 {
		t.Fatal("backend sockets were replaced")
	}
	checkNoise()
	relay.profile.Store(&weakProfile{})
	finishWeakStream(t, f, anchor)
	t.Logf(
		("150ms +/-100ms one-way delay, 15%% segment retransmission at 200ms: %d delayed " +
			"segments; no reconnect"),
		relay.losses.Load(),
	)
}

func TestWeakNetworkShortDirectionalBlackholes(t *testing.T) {
	testWeakNetworkShortDirectionalBlackholes(t, nil, "")
}

func testWeakNetworkShortDirectionalBlackholes(t *testing.T, configure func(*Options), mode string) {
	for _, direction := range []int{0, 1} {
		t.Run([]string{"uplink", "downlink"}[direction], func(t *testing.T) {
			opts := Options{
				Heartbeat:        100 * time.Millisecond,
				HeartbeatTimeout: 800 * time.Millisecond,
				DialTimeout:      400 * time.Millisecond,
				WriteTimeout:     400 * time.Millisecond,
				BufferBytes:      128 * 1024,
				Logf:             func(string, ...any) {},
			}
			if configure != nil {
				configure(&opts)
			}
			f, relay := weakFixtureMode(t, opts, 4*time.Second, 4*time.Second, mode)
			warmupRecoveryNoise(t, f)
			app := f.dial()
			if e := exchange(app, []byte("baseline")); e != nil {
				t.Fatal(e)
			}
			relay.profile.Store(&weakProfile{
				delay:       30 * time.Millisecond,
				jitter:      15 * time.Millisecond,
				lossPercent: 10,
				retransmit:  60 * time.Millisecond,
			})
			accepted := relay.accepted.Load()
			beforeUp, beforeDown := relay.upBytes.Load(), relay.downBytes.Load()
			relay.blackhole[direction].Store(true)
			payload := make([]byte, 128*1024)
			_, _ = cryptorand.Read(payload)
			done := make(chan error, 1)
			go func() { done <- exchange(app, payload) }()
			time.Sleep(250 * time.Millisecond)
			if direction == 0 && relay.downBytes.Load() <= beforeDown {
				t.Fatal("opposite downlink did not remain active")
			}
			if direction == 1 && relay.upBytes.Load() <= beforeUp {
				t.Fatal("opposite uplink did not remain active")
			}
			relay.blackhole[direction].Store(false)
			select {
			case e := <-done:
				if e != nil {
					t.Fatal(e)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("short one-way outage stalled data")
			}
			if relay.accepted.Load() != accepted {
				t.Fatal("brief one-way stall falsely triggered heartbeat reconnect")
			}
			relay.profile.Store(&weakProfile{})
			finishWeakStream(t, f, app)
		})
	}
}

func TestWeakNetworkDirectionalOutagesResumeWithoutDuplicates(t *testing.T) {
	testWeakNetworkDirectionalOutagesResumeWithoutDuplicates(t, nil, "")
}

func testWeakNetworkDirectionalOutagesResumeWithoutDuplicates(t *testing.T, configure func(*Options), mode string) {
	for _, direction := range []int{0, 1} {
		t.Run([]string{"uplink", "downlink"}[direction], func(t *testing.T) {
			opts := Options{
				Heartbeat:        80 * time.Millisecond,
				HeartbeatTimeout: 500 * time.Millisecond,
				DialTimeout:      300 * time.Millisecond,
				WriteTimeout:     300 * time.Millisecond,
				BufferBytes:      128 * 1024,
				Logf:             func(string, ...any) {},
			}
			if configure != nil {
				configure(&opts)
			}
			f, relay := weakFixtureMode(t, opts, 5*time.Second, 5*time.Second, mode)
			warmupRecoveryNoise(t, f)
			app := f.dial()
			baseline := []byte("baseline")
			if e := exchange(app, baseline); e != nil {
				t.Fatal(e)
			}
			client, remote := f.clientSession(), f.remoteSession()
			accepted := relay.accepted.Load()
			relay.profile.Store(&weakProfile{
				delay:       20 * time.Millisecond,
				jitter:      10 * time.Millisecond,
				lossPercent: 10,
				retransmit:  50 * time.Millisecond,
			})
			relay.blackhole[direction].Store(true)
			payload := make([]byte, 256*1024)
			_, _ = cryptorand.Read(payload)
			done := make(chan error, 1)
			go func() { done <- exchange(app, payload) }()
			waitFor(
				t,
				"heartbeat reconnect under one-way outage",
				func() bool { return relay.accepted.Load() > accepted },
			)
			relay.blackhole[direction].Store(false)
			select {
			case e := <-done:
				if e != nil {
					t.Fatal(e)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("one-way outage failed to resume")
			}
			if f.clientSession() != client || f.remoteSession() != remote || f.backendCount.accepted.Load() != 1 {
				t.Fatal("resumable outage replaced the application socket")
			}
			if n := f.backendBytes.Load(); n != int64(len(payload)+len(baseline)) {
				t.Fatalf("upstream bytes lost or repeated: %d", n)
			}
			relay.profile.Store(&weakProfile{})
			finishWeakStream(t, f, app)
		})
	}
}

func TestWeakNetworkDirectionalExpiryReleasesBothSides(t *testing.T) {
	testWeakNetworkDirectionalExpiryReleasesBothSides(t, nil, "")
}

func testWeakNetworkDirectionalExpiryReleasesBothSides(t *testing.T, configure func(*Options), mode string) {
	for _, direction := range []int{0, 1} {
		t.Run([]string{"uplink", "downlink"}[direction], func(t *testing.T) {
			opts := Options{
				Heartbeat:        80 * time.Millisecond,
				HeartbeatTimeout: 400 * time.Millisecond,
				DialTimeout:      200 * time.Millisecond,
				WriteTimeout:     200 * time.Millisecond,
				BufferBytes:      64 * 1024,
				Logf:             func(string, ...any) {},
			}
			if configure != nil {
				configure(&opts)
			}
			f, relay := weakFixtureMode(t, opts, 300*time.Millisecond, 500*time.Millisecond, mode)
			warmupRecoveryNoise(t, f)
			app := f.dial()
			if e := exchange(app, []byte("baseline")); e != nil {
				t.Fatal(e)
			}
			client, remote := f.clientSession(), f.remoteSession()
			relay.blackhole[direction].Store(true)
			f.fillQueues(app, client, remote)
			assertReleased(t, "one-way offline client cleanup", client)
			assertReleased(t, "one-way expired server cleanup", remote)
			// With only the uplink blocked, downstream bytes can already be in the
			// application's TCP receive buffer. Drain that delivered prefix before
			// checking EOF/reset; closing a socket cannot retract delivered bytes.
			_ = app.SetReadDeadline(time.Now().Add(time.Second))
			delivered, err := io.ReadAll(io.LimitReader(app, 256*1024+1))
			if len(delivered) > 256*1024 {
				t.Fatal("duplicate downstream data before expiry")
			}
			if err, ok := err.(net.Error); ok && err.Timeout() {
				t.Fatal("expired application socket remained open")
			}
			if !bytes.Equal(delivered, bytes.Repeat([]byte{0xdb}, len(delivered))) {
				t.Fatal("corrupted delivered downstream prefix")
			}
			waitFor(
				t,
				"one-way expired application sockets",
				func() bool { return f.backendCount.active.Load() == 0 && f.localCount.active.Load() == 0 },
			)
			relay.blackhole[direction].Store(false)
			f.assertFreshConnection()
		})
	}
}

type pacedWriterConn struct {
	net.Conn
	slow *atomic.Bool
}

func (c pacedWriterConn) Write(p []byte) (int, error) {
	if c.slow.Load() {
		time.Sleep(5 * time.Millisecond)
	}
	return c.Conn.Write(p)
}
func TestHeartbeatNotStarvedByBusyWriter(t *testing.T) {
	testHeartbeatNotStarvedByBusyWriter(t, nil)
}

func testHeartbeatNotStarvedByBusyWriter(t *testing.T, configure func(*Options)) {
	var pings, messages atomic.Int64
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, e := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if e != nil {
			return
		}
		defer ws.Close()
		ws.SetPingHandler(func(string) error {
			pings.Add(1)
			return nil
		})
		for {
			if _, p, e := ws.ReadMessage(); e != nil {
				return
			} else if len(p) > chunkSize {
				messages.Add(1) // Only queued full-size application frames, excluding padding.
			}
		}
	}))
	defer peer.Close()
	var slow atomic.Bool
	dialer := websocket.Dialer{NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, e := (&net.Dialer{}).DialContext(ctx, network, addr)
		if e != nil {
			return nil, e
		}
		return pacedWriterConn{c, &slow}, nil
	}}
	ws, _, e := dialer.Dial("ws"+strings.TrimPrefix(peer.URL, "http"), nil)
	if e != nil {
		t.Fatal(e)
	}
	defer ws.Close()
	slow.Store(true)
	opts := Options{
		Heartbeat:        10 * time.Millisecond,
		HeartbeatTimeout: time.Second,
		DialTimeout:      200 * time.Millisecond,
		WriteTimeout:     200 * time.Millisecond,
		BufferBytes:      2 * 1024 * 1024,
		Logf:             func(string, ...any) {},
	}
	if configure != nil {
		configure(&opts)
	}
	opts, e = opts.defaults()
	if e != nil {
		t.Fatal(e)
	}
	s := newSession(context.Background(), "", opts)
	defer s.close()
	for i := 0; i < 64; i++ {
		if e := s.enqueue(opData, 1, make([]byte, chunkSize)); e != nil {
			t.Fatal(e)
		}
	}
	done := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- s.writeTransport(ws, newCodec(bytes.Repeat([]byte{7}, 32), "id", "client", "server", true), done, nil)
	}()
	defer func() {
		close(done)
		ws.Close()
		select {
		case <-result:
		case <-time.After(time.Second):
			t.Error("busy writer did not terminate")
		}
	}()
	waitFor(t, "heartbeats while sending queued data", func() bool { return pings.Load() >= 3 })
	if messages.Load() >= 64 {
		t.Fatal("heartbeat probes were starved until the entire data queue was sent")
	}
	if recoveryNoiseEnabled(opts) && recoveryNoiseSent(s) == 0 {
		t.Fatal("busy writer did not actually send padding")
	}
}

func TestWeakNetworkTLSLatencyJitterAndRetransmission(t *testing.T) {
	opts := Options{
		Heartbeat:        100 * time.Millisecond,
		HeartbeatTimeout: 2 * time.Second,
		DialTimeout:      time.Second,
		WriteTimeout:     time.Second,
		BufferBytes:      128 * 1024,
		Logf:             func(string, ...any) {},
	}
	f, relay := weakFixtureMode(t, opts, 8*time.Second, 8*time.Second, "tls")
	app := f.dial()
	if e := exchange(app, []byte("baseline TLS")); e != nil {
		t.Fatal(e)
	}
	accepted := relay.accepted.Load()
	relay.profile.Store(&weakProfile{
		delay:       150 * time.Millisecond,
		jitter:      100 * time.Millisecond,
		lossPercent: 15,
		retransmit:  200 * time.Millisecond,
	})
	time.Sleep(2200 * time.Millisecond)
	p := make([]byte, 64*1024)
	_, _ = cryptorand.Read(p)
	if e := exchange(app, p); e != nil {
		t.Fatal(e)
	}
	if relay.accepted.Load() != accepted || f.backendCount.accepted.Load() != 1 {
		t.Fatal("healthy TLS weak-network connection reconnected")
	}
	if relay.losses.Load() == 0 {
		t.Fatal("no TLS retransmission delay was injected")
	}
	relay.profile.Store(&weakProfile{})
	finishWeakStream(t, f, app)
}

func (r *weakRelay) traceProbe(c net.Conn) *weakPair {
	r.mu.Lock()
	defer r.mu.Unlock()
	for p := range r.pairs {
		if p.a.RemoteAddr().String() == c.LocalAddr().String() {
			p.tracked.Store(true)
			if os.Getenv("SGATE_STRESS_PROBE_TRACE") == "1" {
				p.trace.Store(true)
				r.logf(
					"probe relay %s registered client=%s frontend=%s backend=%s server=%s",
					time.Now().Format(time.RFC3339Nano),
					p.a.RemoteAddr(),
					p.a.LocalAddr(),
					p.b.LocalAddr(),
					p.b.RemoteAddr(),
				)
			}
			return p
		}
	}
	return nil
}

func (r *weakRelay) traceStatus() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for p := range r.pairs {
		if p.trace.Load() {
			r.logf(
				("probe counters client=%s backend=%s relayConn=%p read=%d/%d written=%d/%d " +
					"blackhole=%t/%t"),
				p.a.RemoteAddr(),
				p.b.LocalAddr(),
				p.b,
				p.read[0].Load(),
				p.read[1].Load(),
				p.written[0].Load(),
				p.written[1].Load(),
				r.blackhole[0].Load(),
				r.blackhole[1].Load(),
			)
		}
	}
}
