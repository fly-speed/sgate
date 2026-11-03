package tunnel

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/logging"
)

// Offsets count repeated STREAM payload, independently of application replay.
// Tracking is bounded; evictions explicitly warn that later duplicates may be
// undercounted. No packet payload or credentials are retained.
const maxQUICTrackedStreams = 2048

type sentExtent struct {
	high  logging.ByteCount
	fin   bool
	stamp uint64
}
type quicSendTracker struct {
	mu      sync.Mutex
	streams map[logging.StreamID]sentExtent
	clock   uint64
}

func (t *quicSendTracker) observe(stats *Stats, frames []logging.Frame) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.streams == nil {
		t.streams = make(map[logging.StreamID]sentExtent)
	}
	retransmit := false
	for _, frame := range frames {
		switch f := frame.(type) {
		case *logging.DataBlockedFrame:
			stats.quicConnectionBlocked.Add(1)
		case *logging.StreamDataBlockedFrame:
			stats.quicStreamBlocked.Add(1)
		case *logging.StreamsBlockedFrame:
			stats.quicStreamCountBlocked.Add(1)
		case *logging.StreamFrame:
			previous, exists := t.streams[f.StreamID]
			if !exists && len(t.streams) >= maxQUICTrackedStreams {
				var oldest logging.StreamID
				stamp := ^uint64(0)
				for id, e := range t.streams {
					if e.stamp < stamp {
						oldest, stamp = id, e.stamp
					}
				}
				delete(t.streams, oldest)
				stats.quicTrackingEvictions.Add(1)
			}
			overlap := min(f.Length, max(logging.ByteCount(0), previous.high-f.Offset))
			if overlap > 0 {
				stats.quicRetransmitted.Add(uint64(overlap))
				retransmit = true
			}
			if f.Fin && previous.fin {
				retransmit = true
			}
			t.clock++
			t.streams[f.StreamID] = sentExtent{high: max(previous.high, f.Offset+f.Length), fin: previous.fin || f.Fin, stamp: t.clock}
		}
	}
	if retransmit {
		stats.quicRetransmitPackets.Add(1)
	}
}
func quicCloseCategory(err error) string {
	var idle *quic.IdleTimeoutError
	var handshake *quic.HandshakeTimeoutError
	var app *quic.ApplicationError
	var transport *quic.TransportError
	switch {
	case errors.As(err, &idle):
		return "idle_timeout"
	case errors.As(err, &handshake):
		return "handshake_timeout"
	case errors.As(err, &app):
		if app.Remote {
			return "remote_application_close"
		}
		return "local_application_close"
	case errors.As(err, &transport):
		return "transport_error"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "other"
	}
}
func (s *session) waitNativeBudget(ch, stopped <-chan struct{}) error {
	if stats := s.opts.Stats; stats != nil {
		start := time.Now()
		stats.quicApplicationBlocked.Add(1)
		defer func() { stats.quicBlockedNanos.Add(uint64(time.Since(start))) }()
	}
	select {
	case <-s.ctx.Done():
		return s.ctx.Err()
	case <-stopped:
		return context.Canceled
	case <-ch:
		return nil
	}
}
