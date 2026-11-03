package tunnel

import (
	"context"
	"sync/atomic"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/logging"
)

// Packet bytes include QUIC headers, encryption, ACKs and retransmissions, but
// exclude UDP/IP headers. They are distinct from the stream counters in Stats.
type QUICSnapshot struct {
	RetransmittedStreamBytes uint64            `json:"retransmitted_stream_bytes"`
	StreamRetransmitPackets  uint64            `json:"stream_retransmit_packets"`
	TrackingEvictions        uint64            `json:"retransmission_tracking_evictions"`
	ConnectionBlockedFrames  uint64            `json:"connection_blocked_frames"`
	StreamBlockedFrames      uint64            `json:"stream_blocked_frames"`
	StreamCountBlockedFrames uint64            `json:"stream_count_blocked_frames"`
	ApplicationBlockedEvents uint64            `json:"application_blocked_events"`
	ApplicationBlockedMS     float64           `json:"application_blocked_ms"`
	PTOEvents                uint64            `json:"pto_events"`
	MinActiveMTU             uint64            `json:"min_active_mtu"`
	CloseReasons             map[string]uint64 `json:"close_reasons"`

	Connections      uint64  `json:"connections_total"`
	Active           int     `json:"active_connections"`
	PacketsTX        uint64  `json:"packets_tx"`
	PacketsRX        uint64  `json:"packets_rx"`
	PacketBytesTX    uint64  `json:"packet_tx_bytes"`
	PacketBytesRX    uint64  `json:"packet_rx_bytes"`
	LostPackets      uint64  `json:"lost_packets"`
	SmoothedRTTMS    float64 `json:"max_smoothed_rtt_ms"`
	CongestionWindow uint64  `json:"congestion_window_bytes"`
	BytesInFlight    uint64  `json:"bytes_in_flight"`
	NativeStreams    uint64  `json:"native_streams_total"`
	ReplayBytes      uint64  `json:"application_replay_bytes"`
}
type quicMetrics struct{ rtt, cwnd, flight, mtu atomic.Uint64 }

func (s *Stats) quicSnapshot() QUICSnapshot {
	result := QUICSnapshot{RetransmittedStreamBytes: s.quicRetransmitted.Load(), StreamRetransmitPackets: s.quicRetransmitPackets.Load(), TrackingEvictions: s.quicTrackingEvictions.Load(), ConnectionBlockedFrames: s.quicConnectionBlocked.Load(), StreamBlockedFrames: s.quicStreamBlocked.Load(), StreamCountBlockedFrames: s.quicStreamCountBlocked.Load(), ApplicationBlockedEvents: s.quicApplicationBlocked.Load(), ApplicationBlockedMS: float64(s.quicBlockedNanos.Load()) / 1e6, PTOEvents: s.quicPTO.Load(), CloseReasons: make(map[string]uint64), Connections: s.quicConnections.Load(), PacketsTX: s.quicPacketsTX.Load(), PacketsRX: s.quicPacketsRX.Load(), PacketBytesTX: s.quicBytesTX.Load(), PacketBytesRX: s.quicBytesRX.Load(), LostPackets: s.quicLost.Load(), NativeStreams: s.quicStreams.Load(), ReplayBytes: s.quicReplay.Load()}
	s.quicMu.Lock()
	defer s.quicMu.Unlock()
	for reason, count := range s.quicCloseReasons {
		result.CloseReasons[reason] = count
	}
	result.Active = len(s.quicActive)
	for m := range s.quicActive {
		if mtu := m.mtu.Load(); mtu > 0 && (result.MinActiveMTU == 0 || mtu < result.MinActiveMTU) {
			result.MinActiveMTU = mtu
		}
		if rtt := float64(m.rtt.Load()) / 1e6; rtt > result.SmoothedRTTMS {
			result.SmoothedRTTMS = rtt
		}
		result.CongestionWindow += m.cwnd.Load()
		result.BytesInFlight += m.flight.Load()
	}
	return result
}
func (s *Stats) quicTracer(_ context.Context, _ logging.Perspective, _ quic.ConnectionID) *logging.ConnectionTracer {
	m := &quicMetrics{}
	tracker := &quicSendTracker{}
	s.quicMu.Lock()
	if s.quicActive == nil {
		s.quicActive = make(map[*quicMetrics]struct{})
	}
	s.quicActive[m] = struct{}{}
	s.quicMu.Unlock()
	s.quicConnections.Add(1)
	tx := func(size logging.ByteCount) { s.quicPacketsTX.Add(1); s.quicBytesTX.Add(uint64(size)) }
	rx := func(size logging.ByteCount) { s.quicPacketsRX.Add(1); s.quicBytesRX.Add(uint64(size)) }
	return &logging.ConnectionTracer{
		SentLongHeaderPacket: func(_ *logging.ExtendedHeader, size logging.ByteCount, _ logging.ECN, _ *logging.AckFrame, frames []logging.Frame) {
			tx(size)
			tracker.observe(s, frames)
		},
		SentShortHeaderPacket: func(_ *logging.ShortHeader, size logging.ByteCount, _ logging.ECN, _ *logging.AckFrame, frames []logging.Frame) {
			tx(size)
			tracker.observe(s, frames)
		},
		ReceivedLongHeaderPacket:  func(_ *logging.ExtendedHeader, size logging.ByteCount, _ logging.ECN, _ []logging.Frame) { rx(size) },
		ReceivedShortHeaderPacket: func(_ *logging.ShortHeader, size logging.ByteCount, _ logging.ECN, _ []logging.Frame) { rx(size) },
		LostPacket:                func(logging.EncryptionLevel, logging.PacketNumber, logging.PacketLossReason) { s.quicLost.Add(1) },
		UpdatedMetrics: func(rtt *logging.RTTStats, cwnd, flight logging.ByteCount, _ int) {
			m.rtt.Store(uint64(rtt.SmoothedRTT()))
			m.cwnd.Store(uint64(cwnd))
			m.flight.Store(uint64(flight))
		},
		UpdatedMTU: func(mtu logging.ByteCount, _ bool) { m.mtu.Store(uint64(mtu)) },
		UpdatedPTOCount: func(value uint32) {
			if value > 0 {
				s.quicPTO.Add(1)
			}
		},
		ClosedConnection: func(err error) {
			s.quicMu.Lock()
			if s.quicCloseReasons == nil {
				s.quicCloseReasons = make(map[string]uint64)
			}
			s.quicCloseReasons[quicCloseCategory(err)]++
			s.quicMu.Unlock()
		},
		Close: func() { s.quicMu.Lock(); delete(s.quicActive, m); s.quicMu.Unlock() },
	}
}
