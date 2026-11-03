package tunnel

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go/logging"
)

func TestResourceReservationAdmissionAndRelease(t *testing.T) {
	opts, err := testOptions().defaults()
	if err != nil {
		t.Fatal(err)
	}
	opts.nativeQUIC = true
	opts.MaxStreams = 2
	opts.ReplayBytes = 128 << 10
	opts.ReceiveBytes = 64 << 10
	opts.NativeStreamBuffer = 32 << 10
	opts.Budget = NewMemoryBudget(opts.sessionReservation())
	first := newSession(context.Background(), "", opts)
	defer first.close()
	rejected := newSession(context.Background(), "", opts)
	if !errors.Is(rejected.initErr, ErrMemoryBudget) {
		rejected.close()
		t.Fatal("budget admission did not reject excess session")
	}
	if err := opts.Budget.Configure(0); err == nil {
		t.Fatal("active reservation limit changed")
	}
	first.close()
	next := newSession(context.Background(), "", opts)
	if next.initErr != nil {
		t.Fatal(next.initErr)
	}
	next.close()
	if snap := opts.Budget.Snapshot(); snap.Reserved != 0 || snap.Peak > snap.Limit || snap.Rejected != 1 {
		t.Fatalf("reservation accounting: %+v", snap)
	}
}
func TestResourceBudgetConcurrentBound(t *testing.T) {
	budget := NewMemoryBudget(1024)
	var wg sync.WaitGroup
	ready := make(chan struct{})
	release := make(chan struct{})
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-ready
			done, err := budget.reserve(64)
			if err == nil {
				<-release
				done()
				done()
			}
		}()
	}
	close(ready)
	// The peak is checked after all reservations and idempotent releases finish.
	close(release)
	wg.Wait()
	if snap := budget.Snapshot(); snap.Peak > 1024 || snap.Reserved != 0 {
		t.Fatalf("budget overflow: %+v", snap)
	}
}
func TestQUICIndependentWindowAndQueueOptions(t *testing.T) {
	opts := testOptions()
	opts.QUICStreamWindow = 128 << 10
	opts.QUICConnectionWindow = 1 << 20
	opts.NativeStreamBuffer = 32 << 10
	opts.ReplayBytes = 256 << 10
	opts.ReceiveBytes = 128 << 10
	normalized, err := opts.defaults()
	if err != nil {
		t.Fatal(err)
	}
	cfg := quicConfig(normalized)
	if cfg.MaxStreamReceiveWindow != 128<<10 || cfg.MaxConnectionReceiveWindow != 1<<20 || normalized.NativeStreamBuffer != 32<<10 || normalized.ReplayBytes != 256<<10 || normalized.ReceiveBytes != 128<<10 {
		t.Fatal("independent resource configuration ignored")
	}
	opts.ReplayBytes = 16 << 10
	if _, err := opts.defaults(); err == nil {
		t.Fatal("stream buffer larger than replay budget accepted")
	}
}
func TestQUICRetransmissionObservationAndBounds(t *testing.T) {
	stats := NewStats()
	tracer := stats.quicTracer(context.Background(), logging.PerspectiveClient, logging.ConnectionID{})
	defer tracer.Close()
	sent := func(offset, length int) {
		tracer.SentShortHeaderPacket(nil, 1200, 0, nil, []logging.Frame{&logging.StreamFrame{StreamID: 4, Offset: logging.ByteCount(offset), Length: logging.ByteCount(length)}})
	}
	sent(0, 100)
	sent(100, 50)
	sent(25, 50)
	sent(125, 50)
	tracer.SentShortHeaderPacket(nil, 10, 0, nil, []logging.Frame{&logging.DataBlockedFrame{}, &logging.StreamDataBlockedFrame{}})
	snap := stats.Snapshot().QUIC
	if snap.RetransmittedStreamBytes != 75 || snap.StreamRetransmitPackets != 2 || snap.ConnectionBlockedFrames != 1 || snap.StreamBlockedFrames != 1 || snap.ReplayBytes != 0 {
		t.Fatalf("network and application retransmissions conflated: %+v", snap)
	}
	tracker := quicSendTracker{}
	for i := 0; i < maxQUICTrackedStreams+1; i++ {
		tracker.observe(stats, []logging.Frame{&logging.StreamFrame{StreamID: logging.StreamID(i), Length: 1}})
	}
	if len(tracker.streams) != maxQUICTrackedStreams || stats.Snapshot().QUIC.TrackingEvictions == 0 {
		t.Fatal("unbounded retransmission history")
	}
}

func TestQUICConnectionBudgetBeforeAuthentication(t *testing.T) {
	seed := httptest.NewTLSServer(nil)
	defer seed.Close()
	roots := x509.NewCertPool()
	roots.AddCert(seed.Certificate())
	opts := testOptions()
	opts.QUICConnectionWindow = 64 << 10
	opts.Budget = NewMemoryBudget(64 << 10)
	listener, err := ListenQUIC("127.0.0.1:0", seed.TLS, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := NewClient(ClientConfig{Key: bytes.Repeat([]byte{8}, 32), Password: testPassword, URL: "quic://" + listener.Addr().String() + "/tunnel", TLSConfig: &tls.Config{RootCAs: roots}, Options: testOptions()})
	if err != nil {
		t.Fatal(err)
	}
	first, err := client.dialQUIC(context.Background(), listener.Addr().String(), "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	second, err := client.dialQUIC(ctx, listener.Addr().String(), "127.0.0.1")
	if err == nil {
		second.Close()
		t.Fatal("pre-authentication window budget bypassed")
	}
	first.(*quicStreamConn).conn.CloseWithError(0, "test release")
	waitFor(t, "QUIC budget released", func() bool { return opts.Budget.Snapshot().Reserved == 0 })
	if opts.Budget.Snapshot().Rejected == 0 {
		t.Fatal("window reservation rejection not recorded")
	}
	next, err := client.dialQUIC(context.Background(), listener.Addr().String(), "127.0.0.1")
	if err != nil {
		t.Fatal("released budget did not permit next connection", err)
	}
	next.Close()
}
