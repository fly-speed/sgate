package tunnel

import (
	"context"
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
