package clientcontrol

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestInputCloseAndStopByteCancelClient(t *testing.T) {
	for _, send := range []bool{false, true} {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cleanup := Watch(ctx, cancel, r)
		if send {
			w.Write([]byte{1})
		} else {
			w.Close()
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
			t.Fatal("control channel did not stop client")
		}
		cleanup()
		w.Close()
		cancel()
	}
}
func TestCancellationUnblocksIdleInput(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cleanup := Watch(ctx, cancel, r)
	cancel()
	done := make(chan struct{})
	go func() { cleanup(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("input watcher leaked on shutdown")
	}
}
