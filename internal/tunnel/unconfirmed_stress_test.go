package tunnel

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	mathrand "math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type unconfirmedEntry struct {
	id    string
	entry *serverEntry
	idle  time.Time
}
type unconfirmedProbes struct {
	mu                                 sync.Mutex
	conns                              []*websocket.Conn
	readers                            sync.WaitGroup
	timeouts, abandoned, silent, noise atomic.Int64
	control, pongs, fragments, partial atomic.Int64
	upgradeRetries                     atomic.Int64
	errors                             chan error
}

func (p *unconfirmedProbes) close() {
	p.mu.Lock()
	conns := p.conns
	p.conns = nil
	p.mu.Unlock()
	for _, ws := range conns {
		ws.Close()
	}
	p.readers.Wait()
}

func (p *unconfirmedProbes) batch(ctx context.Context, f stressLifecycleContext, entries []unconfirmedEntry) {
	if f.kind == "tls-timeout" {
		f.relay.blackhole[1].Store(true)
		defer f.relay.blackhole[1].Store(false)
	}
	var wg sync.WaitGroup
	for i, entry := range entries {
		wg.Add(1)
		go func(i int, e unconfirmedEntry) {
			defer wg.Done()
			if f.kind == "tls-timeout" {
				u, _ := url.Parse(f.probeURL)
				raw, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "tcp", u.Host)
				if err != nil {
					if ctx.Err() == nil {
						p.errors <- err
					}
					return
				}
				defer raw.Close()
				cfg := f.probeTLS.Clone()
				cfg.ServerName = u.Hostname()
				c := tls.Client(raw, cfg)
				hctx, cancel := context.WithTimeout(ctx, time.Second)
				err = c.HandshakeContext(hctx)
				if errors.Is(hctx.Err(), context.DeadlineExceeded) {
					p.timeouts.Add(1)
				} else if ctx.Err() == nil {
					p.errors <- fmt.Errorf("expected TLS handshake deadline, got %v", err)
				}
				cancel()
				return
			}
			// Resume probes preserve the retained session's negotiated capabilities.
			probeURL, _ := url.Parse(f.probeURL)
			if e.entry.session.opts.reportStreamErrors {
				query := probeURL.Query()
				query.Set(streamErrorsQuery, "1")
				probeURL.RawQuery = query.Encode()
			}
			var nonce, stamp string
			var headers http.Header
			var ws *websocket.Conn
			var response *http.Response
			minRetry, minErr := time.ParseDuration(f.report.ReconnectMin)
			maxRetry, maxErr := time.ParseDuration(f.report.ReconnectMax)
			if minErr != nil || maxErr != nil || minRetry <= 0 || maxRetry < minRetry {
				p.errors <- fmt.Errorf("invalid probe reconnect backoff")
				return
			}
			// Failed handshakes retry like the real client, but every attempt
			// shares the original retained session's hard deadline.
			err := retryUnconfirmedUpgrade(ctx, e.idle.Add((*f.server).cfg.SessionTTL), minRetry, maxRetry, func(attemptCtx context.Context) error {
				nonce, stamp = randomID(), strconv.FormatInt(time.Now().Unix(), 10)
				var err error
				headers, err = requestHeaders(f.serverConfig.Key, f.serverConfig.Password, e.id, nonce, stamp, "1", probeURL.RequestURI())
				if err != nil {
					return err
				}
				f.probeNonces.Store(nonce, true)
				defer f.probeNonces.Delete(nonce)
				dialer := websocket.Dialer{TLSClientConfig: f.probeTLS, HandshakeTimeout: (*f.server).cfg.Options.DialTimeout}
				ws, response, err = dialer.DialContext(attemptCtx, probeURL.String(), headers)
				if err != nil && response != nil {
					response.Body.Close()
				}
				return err
			}, func() { p.upgradeRetries.Add(1) })
			if err != nil {
				// The final abandoned-upgrade batch is deliberately scheduled
				// one second before expiry, so losing that race is expected.
				lateAbandoned := f.kind == "abandoned" && !time.Now().Before(e.idle.Add((*f.server).cfg.SessionTTL))
				if ctx.Err() == nil && !lateAbandoned {
					p.errors <- fmt.Errorf("unconfirmed upgrade did not succeed by original expiry: %w", err)
				}
				return
			}
			serverNonce := response.Header.Get("X-Sgate-Nonce")
			expected := mac(
				f.serverConfig.Key,
				protocol,
				"server",
				e.id,
				nonce,
				serverNonce,
				stamp,
				headers.Get(passwordHeader),
			)
			if !hmac.Equal([]byte(expected), []byte(response.Header.Get("X-Sgate-Auth"))) {
				ws.Close()
				p.errors <- fmt.Errorf("unconfirmed server authentication failed")
				return
			}
			if f.kind == "abandoned" {
				p.abandoned.Add(1)
				ws.Close()
				return
			}
			p.silent.Add(1)
			var relayPair *weakPair
			if f.relay != nil {
				relayPair = f.relay.traceProbe(ws.UnderlyingConn())
			}
			p.mu.Lock()
			p.conns = append(p.conns, ws)
			p.mu.Unlock()
			active := f.kind == "control" || f.kind == "fragmented"
			if !active && i%2 == 0 {
				return
			} // Completely silent: no reads, writes or Pong replies.
			// Drain server frames in active probes and the other half of silent probes.
			// None sends a complete encrypted ACK, DATA or NOISE to confirm the resume.
			codec := newCodec(f.serverConfig.Key, e.id, nonce, serverNonce, true)
			deadline := e.idle.Add((*f.server).cfg.SessionTTL)
			reportEarly := func(err error) {
				if ctx.Err() == nil && time.Now().Before(deadline.Add(-time.Second)) {
					if relayPair != nil {
						err = fmt.Errorf(
							"pair-close=%v read=%d/%d written=%d/%d: %w",
							relayPair.closeCause.Load(),
							relayPair.read[0].Load(),
							relayPair.read[1].Load(),
							relayPair.written[0].Load(),
							relayPair.written[1].Load(),
							err,
						)
					}
					p.errors <- fmt.Errorf(
						"%s probe stopped at %s with %s remaining, client=%s: %w",
						f.kind,
						time.Now().Format(time.RFC3339Nano),
						time.Until(deadline).Round(time.Millisecond),
						ws.LocalAddr(),
						err,
					)
				}
			}
			var writeMu sync.Mutex
			ws.SetPongHandler(func(string) error {
				p.pongs.Add(1)
				return nil
			})
			// Raw fragmented writes and control replies share a mutex so their
			// TLS writes cannot interleave WebSocket frame bytes.
			writeBudget := (*f.server).cfg.Options.WriteTimeout
			ws.SetPingHandler(func(data string) error {
				writeMu.Lock()
				defer writeMu.Unlock()
				return ws.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(writeBudget))
			})
			if active {
				p.readers.Add(1)
				go func() {
					defer p.readers.Done()
					ticker := time.NewTicker(250 * time.Millisecond)
					defer ticker.Stop()
					payload := codec.seal((frame{op: opNoise, data: make([]byte, 8192)}).encode())
					offset := 0
					for {
						select {
						case <-ctx.Done():
							return
						case <-ticker.C:
						}
						var err error
						writeMu.Lock()
						if f.kind == "control" {
							for _, typ := range []int{websocket.PingMessage, websocket.PongMessage} {
								err = ws.WriteControl(typ, []byte("unconfirmed"), time.Now().Add(writeBudget))
								if err != nil {
									break
								}
								p.control.Add(1)
							}
						} else {
							// Every frame has FIN=0. Even after two minutes the
							// encrypted message lacks most bytes and its GCM tag.
							op := byte(0)
							if offset == 0 {
								op = websocket.BinaryMessage
							}
							if offset+8 >= len(payload) {
								err = fmt.Errorf("incomplete-frame probe exhausted payload")
							} else {
								_ = ws.UnderlyingConn().SetWriteDeadline(time.Now().Add(writeBudget))
								err = writeUnconfirmedFragment(ws.UnderlyingConn(), op, payload[offset:offset+8])
								if err == nil {
									offset += 8
									p.fragments.Add(1)
									p.partial.Add(8)
								}
							}
						}
						writeMu.Unlock()
						if err != nil {
							reportEarly(err)
							return
						}
					}
				}()
			}
			p.readers.Add(1)
			go func() {
				defer p.readers.Done()
				for {
					typ, data, err := ws.ReadMessage()
					if err != nil {
						if active {
							reportEarly(err)
						}
						return
					}
					if typ != websocket.BinaryMessage {
						p.errors <- fmt.Errorf("unexpected probe message type")
						return
					}
					plain, err := codec.open(data)
					if err != nil {
						p.errors <- err
						return
					}
					frame, err := decodeFrame(plain)
					if err != nil {
						p.errors <- err
						return
					}
					if frame.op == opNoise {
						p.noise.Add(1)
					}
				}
			}()
		}(i, entry)
	}
	wg.Wait()
}

func stressUnconfirmedRecovery(
	t *testing.T,
	f stressLifecycleContext,
	oldProbe *stressLifecycleProbe,
	start time.Time,
	clients, remotes []*session,
) {
	t.Helper()
	s := *f.server
	retention := s.cfg.SessionTTL
	f.report.UnconfirmedProbeWriteTimeout = s.cfg.Options.WriteTimeout.String()
	stressWaitFor(t, "last confirmed handlers detached before retention snapshot", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, e := range s.sessions {
			if e.inUse {
				return false
			}
		}
		return true
	})
	var entries []unconfirmedEntry
	s.mu.Lock()
	for id, e := range s.sessions {
		entries = append(entries, unconfirmedEntry{id, e, e.idle})
	}
	s.mu.Unlock()
	if len(entries) != len(clients) {
		t.Fatal("missing old sessions for unconfirmed probes")
	}
	lastDeadline := start
	for _, e := range entries {
		if d := e.idle.Add(retention); d.After(lastDeadline) {
			lastDeadline = d
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &unconfirmedProbes{errors: make(chan error, 8*len(entries))}
	manualDone := make(chan struct{})
	defer func() {
		cancel()
		<-manualDone
		p.close()
	}()
	go func() {
		defer close(manualDone)
		schedule := []time.Duration{0}
		if f.kind == "tls-timeout" || f.kind == "abandoned" {
			schedule = []time.Duration{0, retention / 2, retention - time.Second}
		}
		for _, at := range schedule {
			timer := time.NewTimer(max(time.Until(start.Add(at)), 0))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			p.batch(ctx, f, entries)
		}
	}()
	all := append(append([]*session(nil), clients...), remotes...)
	logAt := time.Now().Add(30 * time.Second)
	sampleAt := time.Now().Add(time.Second)
	traced := false
	stressWaitForBudget(t, "unconfirmed old sessions expire at original deadlines", time.Until(lastDeadline)+5*time.Second, func() bool {
		s.mu.Lock()
		unchanged := true
		for _, e := range entries {
			if !e.entry.idle.Equal(e.idle) {
				unchanged = false
			}
		}
		s.mu.Unlock()
		if !unchanged {
			t.Fatal("unconfirmed reconnect extended old retention timestamp")
		}
		clean := true
		for _, old := range all {
			old.mu.Lock()
			closed, pending, limit := old.closed, old.pendingBytes, old.opts.BufferBytes
			old.mu.Unlock()
			if pending > limit {
				t.Fatal("unconfirmed replay queues exceeded bounds")
			}
			if closed && time.Since(start) < retention-time.Second {
				t.Fatal("unconfirmed test ended old retention early")
			}
			clean = clean && sessionReleased(old)
		}
		if clean {
			// Record observation before optional telemetry or logging adds delay.
			f.report.UnconfirmedReleaseMilliseconds = time.Since(start).Milliseconds()
			return true
		}
		if !traced && os.Getenv("SGATE_STRESS_PROBE_TRACE") == "1" && time.Since(start) > 20*time.Second {
			traced = true
			if f.relay != nil {
				f.relay.traceStatus()
			}
			buf := make([]byte, 16<<20)
			n := runtime.Stack(buf, true)
			if path := os.Getenv("SGATE_STRESS_REPORT"); path != "" {
				if out, err := exec.Command("netstat", "-anv", "-p", "tcp").Output(); err == nil {
					_ = os.WriteFile(path+".probe-tcp", out, 0644)
				}
				if err := os.WriteFile(path+".probe-stacks", buf[:n], 0644); err != nil {
					t.Error(err)
				}
			}
		}
		if time.Now().After(sampleAt) {
			f.sample("unconfirmed-progress", f.round, false)
			sampleAt = time.Now().Add(time.Second)
		}
		if time.Now().After(logAt) {
			t.Logf(
				"%s offline %s: all old retention timestamps unchanged; caches bounded",
				f.kind,
				time.Since(start).Round(time.Second),
			)
			logAt = time.Now().Add(30 * time.Second)
		}
		return false
	})
	select {
	case <-manualDone:
	case <-time.After(6 * time.Second):
		t.Fatal("unconfirmed attempt workers did not stop")
	}
	p.close()
	leftoverNonce := false
	f.probeNonces.Range(func(_, _ any) bool {
		leftoverNonce = true
		return false
	})
	if leftoverNonce {
		t.Fatal("probe authorization entries were not released")
	}
	close(p.errors)
	for err := range p.errors {
		t.Error(err)
	}
	f.report.UnconfirmedUpgradeRetries += p.upgradeRetries.Load()
	f.report.UnconfirmedControlSent += p.control.Load()
	f.report.UnconfirmedPongsReceived += p.pongs.Load()
	f.report.UnconfirmedFragmentsSent += p.fragments.Load()
	f.report.UnconfirmedPartialBytes += p.partial.Load()
	f.report.TLSHandshakeTimeouts += p.timeouts.Load()
	f.report.AbandonedUpgrades += p.abandoned.Load()
	f.report.SilentUpgrades += p.silent.Load()
	f.report.UnconfirmedNoiseReceived += p.noise.Load()
	if f.kind == "tls-timeout" && p.timeouts.Load() < int64(2*len(entries)) {
		t.Fatal("insufficient actual TLS handshake timeouts")
	}
	if f.kind == "abandoned" && p.abandoned.Load() < int64(2*len(entries)) {
		t.Fatal("insufficient actual abandoned upgrades")
	}
	if (f.kind == "silent" || f.kind == "control" || f.kind == "fragmented") &&
		(p.silent.Load() != int64(len(entries)) || p.noise.Load() == 0) {
		t.Fatal("silent probes did not cover every old session and encrypted padding")
	}
	if f.kind == "control" &&
		(p.control.Load() < int64(len(entries)*10) || p.pongs.Load() < int64(len(entries)*3)) {
		t.Fatal("continuous control probes did not exchange enough Ping/Pong frames")
	}
	if f.kind == "fragmented" && p.fragments.Load() < int64(len(entries)*10) {
		t.Fatal("incomplete encrypted frames were not sent in enough fragments")
	}
	done := make(chan struct{})
	go func() {
		oldProbe.wg.Wait()
		close(done)
	}()
	cleanupDeadline := time.Now().Add(20 * time.Second)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Log(("application close notification exceeded five seconds; retaining the 20-second stress " +
			"cleanup budget"))
		if path := os.Getenv("SGATE_STRESS_REPORT"); path != "" {
			buf := make([]byte, 16<<20)
			n := runtime.Stack(buf, true)
			_ = os.WriteFile(path+".expiry-stacks", buf[:n], 0644)
			var connections []map[string]any
			oldProbe.connections.Range(func(key, value any) bool {
				c := key.(net.Conn)
				connections = append(
					connections,
					map[string]any{"local": c.LocalAddr().String(), "remote": c.RemoteAddr().String(), "backend": value},
				)
				return true
			})
			data, _ := json.MarshalIndent(connections, "", "  ")
			_ = os.WriteFile(path+".expiry-connections.json", data, 0644)
			snapshotCtx, snapshotCancel := context.WithTimeout(context.Background(), time.Second)
			if out, err := exec.CommandContext(snapshotCtx, "netstat", "-anv", "-p", "tcp").Output(); err == nil {
				_ = os.WriteFile(path+".expiry-tcp", out, 0644)
			}
			snapshotCancel()
		}
		select {
		case <-done:
		case <-time.After(max(time.Until(cleanupDeadline), 0)):
			t.Fatal("old application writers did not exit within stress cleanup budget")
		}
	}
	f.report.UnconfirmedApplicationReleaseMilliseconds = time.Since(start).Milliseconds()
	close(oldProbe.errors)
	for err := range oldProbe.errors {
		t.Error(err)
	}
	stressWaitFor(
		t,
		"old unconfirmed application connections released offline",
		func() bool { return f.backendCount.active.Load() == 0 && f.localCount.active.Load() == 0 },
	)
	f.sample(f.kind+"-offline-expired", f.round, true)
	t.Logf(
		("%s: original retention held; sessions, caches and transport timers released at %s; " +
			"application peers released by %s; TLS timeouts=%d abandoned=%d silent=%d noise " +
			"received=%d"),
		f.kind,
		time.Duration(f.report.UnconfirmedReleaseMilliseconds)*time.Millisecond,
		time.Duration(f.report.UnconfirmedApplicationReleaseMilliseconds)*time.Millisecond,
		p.timeouts.Load(),
		p.abandoned.Load(),
		p.silent.Load(),
		p.noise.Load(),
	)
	f.offline.Store(false)
	stressWaitFor(t, "fresh sessions after unconfirmed attempts", f.attached)
	f.peer.Store(nil)
	f.sample(f.kind+"-recovered", f.round, true)
}

// writeUnconfirmedFragment emits a valid masked client WebSocket fragment with
// FIN unset; using the public message writer would eventually finish the message.
func writeUnconfirmedFragment(w io.Writer, opcode byte, payload []byte) error {
	if len(payload) > 125 {
		return fmt.Errorf("probe fragment too large")
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	data := append([]byte{opcode, 0x80 | byte(len(payload))}, mask[:]...)
	for i, b := range payload {
		data = append(data, b^mask[i%4])
	}
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

// Only pre-upgrade failures are retried. A successfully upgraded probe that
// stops early still fails coverage/integrity checks in batch.
func retryUnconfirmedUpgrade(
	parent context.Context,
	deadline time.Time,
	minimum, maximum time.Duration,
	attempt func(context.Context) error,
	retried func(),
) error {
	ctx, cancel := context.WithDeadline(parent, deadline)
	defer cancel()
	backoff := minimum
	var last error
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("last upgrade error %v: %w", last, err)
		}
		last = attempt(ctx)
		if last == nil {
			return nil
		}
		retried()
		delay := backoff/2 + time.Duration(mathrand.Int64N(int64(backoff/2)+1))
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("last upgrade error %v: %w", last, ctx.Err())
		case <-timer.C:
		}
		if backoff < maximum/2 {
			backoff *= 2
		} else {
			backoff = maximum
		}
	}
}

func TestUnconfirmedUpgradeRetryKeepsOriginalDeadline(t *testing.T) {
	t.Run("transient failures recover", func(t *testing.T) {
		deadline := time.Now().Add(time.Second)
		attempts, retries := 0, 0
		err := retryUnconfirmedUpgrade(context.Background(), deadline, time.Millisecond, 2*time.Millisecond, func(ctx context.Context) error {
			actual, ok := ctx.Deadline()
			if !ok || !actual.Equal(deadline) {
				t.Fatal("retry changed original deadline")
			}
			attempts++
			if attempts < 3 {
				return errors.New("transient handshake failure")
			}
			return nil
		}, func() { retries++ })
		if err != nil || attempts != 3 || retries != 2 {
			t.Fatalf("attempts=%d retries=%d err=%v", attempts, retries, err)
		}
	})
	t.Run("expiry interrupts backoff", func(t *testing.T) {
		started := time.Now()
		attempts := 0
		err := retryUnconfirmedUpgrade(
			context.Background(),
			started.Add(40*time.Millisecond),
			time.Second,
			time.Second,
			func(ctx context.Context) error {
				attempts++
				return errors.New("offline")
			},
			func() {},
		)
		if !errors.Is(err, context.DeadlineExceeded) || attempts != 1 ||
			time.Since(started) > 500*time.Millisecond {
			t.Fatalf("attempts=%d elapsed=%s err=%v", attempts, time.Since(started), err)
		}
	})
	t.Run("cancellation stops retry", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		attempts := 0
		err := retryUnconfirmedUpgrade(
			ctx,
			time.Now().Add(time.Hour),
			time.Second,
			time.Second,
			func(context.Context) error {
				attempts++
				cancel()
				return errors.New("offline")
			},
			func() {},
		)
		if !errors.Is(err, context.Canceled) || attempts != 1 {
			t.Fatalf("attempts=%d err=%v", attempts, err)
		}
	})
}
