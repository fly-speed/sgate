package tunnel

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestRetransmissionBufferBackpressureAndCancellation(t *testing.T) {
	opts, err := testOptions().defaults()
	if err != nil {
		t.Fatal(err)
	}
	opts.BufferBytes = chunkSize + frameHeader
	s := newSession(context.Background(), "", opts)
	defer s.close()
	if e := s.enqueue(opData, 1, make([]byte, chunkSize)); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- s.enqueue(opData, 1, []byte("next")) }()
	select {
	case <-done:
		t.Fatal("full buffer did not apply backpressure")
	case <-time.After(25 * time.Millisecond):
	}
	if e := s.acknowledge(2); e == nil {
		t.Fatal("accepted ACK beyond end")
	}
	if e := s.acknowledge(1); e != nil {
		t.Fatal(e)
	}
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("ACK did not release buffer")
	}
	// Fill remaining space and ensure shutdown unblocks a waiting producer.
	if e := s.enqueue(opData, 1, make([]byte, chunkSize-30)); e != nil {
		t.Fatal(e)
	}
	go func() { done <- s.enqueue(opData, 1, make([]byte, chunkSize)) }()
	s.close()
	select {
	case e := <-done:
		if e == nil {
			t.Fatal("enqueue after close succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("close did not unblock producer")
	}
}

func TestResetReleasesBlockedStreamReader(t *testing.T) {
	opts, _ := testOptions().defaults()
	opts.BufferBytes = chunkSize + frameHeader
	s := newSession(context.Background(), "", opts)
	defer s.close()
	if err := s.enqueue(opData, 99, make([]byte, chunkSize)); err != nil {
		t.Fatal(err)
	}
	c, peer := net.Pipe()
	defer peer.Close()
	st := &stream{conn: c}
	s.mu.Lock()
	s.streams[1] = st
	s.mu.Unlock()
	done := make(chan struct{})
	go func() {
		s.readStream(1, st)
		close(done)
	}()
	// Write returns once readStream has read the bytes and is trying to enqueue them.
	if _, err := peer.Write([]byte("blocked data")); err != nil {
		t.Fatal(err)
	}
	s.drop(1)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reset left a reader waiting on unrelated ACKs")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.next != 1 || len(s.pending) != 1 {
		t.Fatal("reset stream queued stale data")
	}
}

func TestResetReleasesBlockedOpen(t *testing.T) {
	opts, _ := testOptions().defaults()
	opts.BufferBytes = chunkSize + frameHeader
	s := newSession(context.Background(), "", opts)
	defer s.close()
	_ = s.enqueue(opData, 99, make([]byte, chunkSize))
	c, peer := net.Pipe()
	defer peer.Close()
	done := make(chan error, 1)
	go func() { done <- s.addLocal(c) }()
	waitFor(
		t,
		"registered stream",
		func() bool {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.streams[1] != nil
		},
	)
	s.drop(1)
	select {
	case err := <-done:
		if !errors.Is(err, errStreamClosed) {
			t.Fatalf("open error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("OPEN producer leaked")
	}
}

func TestAcknowledgementReleasesEmptyQueue(t *testing.T) {
	opts, _ := testOptions().defaults()
	s := newSession(context.Background(), "", opts)
	defer s.close()
	for i := 0; i < 100; i++ {
		if err := s.enqueue(opData, 1, []byte("data")); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.acknowledge(100); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending != nil || s.pendingBytes != 0 {
		t.Fatalf(
			"empty retransmission queue retained storage: len=%d cap=%d bytes=%d",
			len(s.pending),
			cap(s.pending),
			s.pendingBytes,
		)
	}
}

func TestParentCancellationClosesSession(t *testing.T) {
	opts, _ := testOptions().defaults()
	ctx, cancel := context.WithCancel(context.Background())
	s := newSession(ctx, "", opts)
	defer s.close()
	c, peer := net.Pipe()
	defer peer.Close()
	s.mu.Lock()
	s.streams[1] = &stream{conn: c}
	s.mu.Unlock()
	_ = s.enqueue(opData, 1, []byte("unacknowledged"))
	cancel()
	waitFor(t, "canceled session cleanup", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.closed && len(s.streams) == 0 && s.pending == nil && s.pendingBytes == 0
	})
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	var b [1]byte
	if _, err := peer.Read(b[:]); err != io.EOF {
		t.Fatalf("socket not closed: %v", err)
	}
	late := newSession(ctx, "", opts)
	defer late.close()
	left, right := net.Pipe()
	defer right.Close()
	if err := late.addLocal(left); err == nil {
		t.Fatal("session accepted stream after cancellation")
	}
}

type failedHalfClose struct{ net.Conn }

func (failedHalfClose) CloseWrite() error { return errors.New("half-close failed") }
func TestHalfCloseFailureResetsStream(t *testing.T) {
	opts, _ := testOptions().defaults()
	s := newSession(context.Background(), "", opts)
	defer s.close()
	c, peer := net.Pipe()
	defer peer.Close()
	s.mu.Lock()
	s.streams[1] = &stream{conn: failedHalfClose{c}}
	s.mu.Unlock()
	if err := s.apply(frame{op: opFin, stream: 1}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "RESET after half-close failure", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.streams[1] == nil && len(s.pending) == 1 && s.pending[0].op == opReset
	})
}

func TestConcurrentResetDoesNotTerminateHealthyStreams(t *testing.T) {
	opts, _ := testOptions().defaults()
	opts.MaxStreams = 2
	opts.BufferBytes = chunkSize + frameHeader
	s := newSession(context.Background(), "", opts)
	defer s.close()
	bad, badPeer := net.Pipe()
	defer badPeer.Close()
	healthy, healthyPeer := net.Pipe()
	defer healthyPeer.Close()
	s.mu.Lock()
	s.streams[1] = &stream{conn: bad}
	s.streams[2] = &stream{conn: healthy}
	s.mu.Unlock()
	if err := s.enqueue(opData, 2, make([]byte, chunkSize)); err != nil {
		t.Fatal(err)
	}
	// A failed write and the reader it wakes can report the same failure together.
	// Repeat those notifications to ensure reset remains idempotent under contention.
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.reset(1)
		}()
	}
	wg.Wait()
	s.mu.Lock()
	closed, live := s.closed, s.streams[2] != nil
	s.mu.Unlock()
	if closed || !live {
		t.Fatal("duplicate resets closed the entire session and its healthy stream")
	}
	if err := s.acknowledge(1); err != nil {
		t.Fatal(err)
	}
	waitFor(
		t,
		"one queued reset",
		func() bool {
			s.mu.Lock()
			defer s.mu.Unlock()
			return len(s.pending) == 1
		},
	)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.next != 2 || s.pending[0].op != opReset || s.pending[0].stream != 1 {
		t.Fatalf("reset was not deduplicated: next=%d pending=%+v", s.next, s.pending)
	}
}
