package tunnel

import (
	"testing"
	"time"
)

func stressBoundaryOrderRemotes(t *testing.T, p *stressLifecycleProbe, remotes []*session, n int) []*session {
	t.Helper()
	ordered := make([]*session, n)
	for _, s := range remotes {
		s.mu.Lock()
		client := -1
		for _, st := range s.streams {
			if v, ok := p.backendClients.Load(st.conn.LocalAddr().String()); ok {
				client = v.(int)
				break
			}
		}
		s.mu.Unlock()
		if client < 0 || client >= n || ordered[client] != nil {
			t.Fatal("could not associate boundary server session with client")
		}
		ordered[client] = s
	}
	for _, s := range ordered {
		if s == nil {
			t.Fatal("missing boundary server session")
		}
	}
	return ordered
}

func stressBoundaryRecovery(
	t *testing.T,
	f stressLifecycleContext,
	p *stressLifecycleProbe,
	start time.Time,
	clients, remotes []*session,
) {
	t.Helper()
	all := append(append([]*session(nil), clients...), remotes...)
	frozen := make([]uint64, len(all))
	for i, s := range all {
		frozen[i] = recoveryNoiseSent(s)
	}
	deadline := start.Add(f.boundaryOutage)
	timer := time.NewTimer(max(time.Until(deadline), 0))
	ticker := time.NewTicker(100 * time.Millisecond)
	defer timer.Stop()
	defer ticker.Stop()
	logAt := start.Add(30 * time.Second)
hold:
	for {
		for i, s := range all {
			s.mu.Lock()
			noise, pending, limit, closed := s.noiseSent, s.pendingBytes, s.opts.BufferBytes, s.closed
			s.mu.Unlock()
			if noise != frozen[i] || pending > limit {
				t.Fatal("boundary outage leaked padding or exceeded replay limit")
			}
			if closed && time.Since(start) < 119*time.Second {
				t.Fatal("boundary session expired before the default window")
			}
		}
		if time.Now().After(logAt) {
			t.Logf(
				"boundary %s offline %s: replay queues bounded and old padding timers stopped",
				f.boundaryOutage,
				time.Since(start).Round(time.Second),
			)
			logAt = time.Now().Add(30 * time.Second)
		}
		select {
		case <-timer.C:
			break hold
		case <-ticker.C:
		}
	}
	f.offline.Store(false)
	f.report.BoundaryRestoreMilliseconds = append(f.report.BoundaryRestoreMilliseconds, time.Since(start).Milliseconds())
	t.Logf(
		"boundary network restored at %s (target %s); TLS handshake and expiry now race",
		time.Since(start).Round(time.Millisecond),
		f.boundaryOutage,
	)
	f.sample("boundary-network-restored", f.round, true)
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(45 * time.Second):
		f.sample("boundary-stuck", f.round, true)
		t.Fatal("old boundary application connections survived resume/expiry resolution")
	}
	close(p.errors)
	for err := range p.errors {
		t.Error(err)
	}
	completeByClient := make([]int, len(clients))
	completeStreams := make(map[uint64]int)
	close(p.results)
	for result := range p.results {
		if result.complete {
			i := int((result.seed >> 32) & 0xffff)
			completeByClient[i]++
			completeStreams[result.seed]++
		}
	}
	for i, client := range clients {
		remote := remotes[i]
		client.mu.Lock()
		clientClosed := client.closed
		client.mu.Unlock()
		remote.mu.Lock()
		remoteClosed := remote.closed
		remote.mu.Unlock()
		if clientClosed || remoteClosed {
			// A failed handshake must not leave the other half holding orphaned
			// backend sockets and replay data for another full retention window.
			stressWaitFor(
				t,
				"both halves of expired boundary session released",
				func() bool { return sessionReleased(client) && sessionReleased(remote) },
			)
			f.report.BoundaryExpiredSessions++
		} else {
			if completeByClient[i] != 2*f.streams {
				t.Fatalf(
					"resumed boundary client %d had truncated data: %d/%d directions",
					i,
					completeByClient[i],
					2*f.streams,
				)
			}
			f.states[i].mu.Lock()
			same := f.states[i].current == client
			f.states[i].mu.Unlock()
			if !same {
				t.Fatal("resumed boundary client replaced its session")
			}
			client.mu.Lock()
			cleanClient := len(client.streams) == 0 && client.pendingBytes == 0
			client.mu.Unlock()
			remote.mu.Lock()
			cleanRemote := len(remote.streams) == 0 && remote.pendingBytes == 0
			remote.mu.Unlock()
			if !cleanClient || !cleanRemote {
				stressWaitFor(t, "resumed boundary streams and replay queues released", func() bool {
					client.mu.Lock()
					a := len(client.streams) == 0 && client.pendingBytes == 0 && client.pending == nil
					client.mu.Unlock()
					remote.mu.Lock()
					b := len(remote.streams) == 0 && remote.pendingBytes == 0 && remote.pending == nil
					remote.mu.Unlock()
					return a && b
				})
			}
			f.report.BoundaryResumedSessions++
		}
	}
	full := 0
	for _, n := range completeStreams {
		if n == 2 {
			full++
		}
	}
	f.report.BoundaryCompleteStreams += full
	f.report.BoundaryInterruptedStreams += len(clients)*f.streams - full
	stressWaitFor(t, "exactly one current server session per boundary client", f.attached)
	stressWaitFor(
		t,
		"all old boundary application sockets released",
		func() bool { return f.backendCount.active.Load() == 0 && f.localCount.active.Load() == 0 },
	)
	f.peer.Store(nil)
	f.sample("boundary-resolved", f.round, true)
	f.report.BoundaryResolveMilliseconds = append(f.report.BoundaryResolveMilliseconds, time.Since(start).Milliseconds())
	t.Logf(
		("boundary %s resolved: resumed=%d expired=%d complete old streams=%d interrupted=%d; " +
			"fresh sessions ready"),
		f.boundaryOutage,
		f.report.BoundaryResumedSessions,
		f.report.BoundaryExpiredSessions,
		full,
		len(clients)*f.streams-full,
	)
}
