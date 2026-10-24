package tunnel

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestCancelledSessionReleases128ReadersWithoutSessionLock(t *testing.T) {
	opts, err := testOptions().defaults()
	if err != nil {
		t.Fatal(err)
	}
	s := newSession(context.Background(), "", opts)
	defer s.close()
	done := make(chan struct{}, 128)

	// close cancels the session and closes all sockets while holding mu.
	// Readers must not queue behind that same lock just to discover shutdown.
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := uint64(1); id <= 128; id++ {
		local, peer := net.Pipe()
		defer peer.Close()
		st := &stream{conn: local}
		s.streams[id] = st
		go func() {
			s.readStream(id, st)
			done <- struct{}{}
		}()
	}
	s.cancel()
	for _, st := range s.streams {
		st.conn.Close()
	}
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for i := 0; i < 128; i++ {
		select {
		case <-done:
		case <-timer.C:
			t.Fatalf("only %d/128 cancelled readers exited while session lock was held", i)
		}
	}
}
