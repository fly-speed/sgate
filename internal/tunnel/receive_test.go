package tunnel

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// 旧端未限制在途帧时，队列满的等待也必须随传输退出，不能占住会话不开始过期。
func TestReceiveQueueWaitStopsWithTransport(t *testing.T) {
	opts, err := testOptions().defaults()
	if err != nil {
		t.Fatal(err)
	}
	opts.BufferBytes = chunkSize + frameHeader
	s := newSession(context.Background(), "", opts)
	defer s.close()
	local, peer := net.Pipe()
	defer peer.Close()
	s.mu.Lock()
	s.streams[1] = &stream{conn: local}
	s.mu.Unlock()
	stopped := make(chan struct{})
	if err := s.queueIncoming(frame{op: opData, seq: 1, stream: 1, data: make([]byte, chunkSize)}, stopped); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.queueIncoming(frame{op: opData, seq: 2, stream: 1, data: []byte("x")}, stopped) }()
	select {
	case err := <-done:
		t.Fatal("queue did not stay bounded", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(stopped)
	select {
	case err := <-done:
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("transport retained blocked admission")
	}
	s.close()
	waitFor(t, "incoming cache released", func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.incoming == nil && s.incomingBytes == 0 })
}
