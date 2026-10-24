package tunnel

import (
	"testing"
	"time"
)

func stressSessionsDetached(sessions []*session) bool {
	for _, s := range sessions {
		if !s.transportMu.TryLock() {
			return false
		}
		s.mu.Lock()
		detached := s.active == nil
		s.mu.Unlock()
		s.transportMu.Unlock()
		if !detached {
			return false
		}
	}
	return true
}

// Runs in the churn worker. Errors are reported without Fatal so main can join
// the worker, restore the network and close every application connection.
func stressHoldRetainedOutage(t *testing.T, duration time.Duration, abort <-chan struct{}, clients, remotes []*session) {
	t.Helper()
	all := append(append([]*session(nil), clients...), remotes...)
	start := time.Now()
	finish := start.Add(duration)
	for !stressSessionsDetached(all) {
		if time.Since(start) > 5*time.Second {
			t.Error("retained transport writer/timer did not stop")
			return
		}
		select {
		case <-abort:
			return
		case <-time.After(10 * time.Millisecond):
		}
	}
	frozen, full := make([]uint64, len(all)), make([]bool, len(all))
	for i, s := range all {
		frozen[i] = recoveryNoiseSent(s)
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	logAt := start.Add(30 * time.Second)
	for {
		for i, s := range all {
			s.mu.Lock()
			closed, pending, noise, limit := s.closed, s.pendingBytes, s.noiseSent, s.opts.BufferBytes
			s.mu.Unlock()
			if closed {
				t.Error("session expired before the within-retention recovery")
				return
			}
			if pending > limit {
				t.Error("retained replay queue exceeded configured bound")
				return
			}
			if noise != frozen[i] {
				t.Error("padding continued while transport was offline")
				return
			}
			full[i] = full[i] || pending > 128*1024
		}
		if !time.Now().Before(finish) {
			break
		}
		if time.Now().After(logAt) {
			t.Logf("retained outage %s: all old sessions alive, replay queues bounded, padding timers stopped", time.Since(start).Round(time.Second))
			logAt = time.Now().Add(30 * time.Second)
		}
		select {
		case <-abort:
			t.Error("application stopped before outage recovery")
			return
		case <-ticker.C:
		}
	}
	for _, seen := range full {
		if !seen {
			t.Error("retained outage did not fill every bidirectional replay queue")
		}
	}
	t.Logf("restoring network after %s within the default two-minute retention window", time.Since(start).Round(time.Millisecond))
}

func stressCheckRetainedIdentity(t *testing.T, states []*clientSessions, server *Server, clients, remotes []*session) {
	t.Helper()
	if len(clients) != len(states) || len(remotes) != len(states) {
		t.Fatal("retention outage was not injected for every client")
	}
	for i, state := range states {
		state.mu.Lock()
		same := state.current == clients[i]
		state.mu.Unlock()
		if !same {
			t.Fatal("within-retention recovery replaced client session")
		}
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	for _, old := range remotes {
		found := false
		for _, e := range server.sessions {
			if e.session == old && e.inUse {
				found = true
			}
		}
		if !found {
			t.Fatal("within-retention recovery replaced server session")
		}
	}
}
