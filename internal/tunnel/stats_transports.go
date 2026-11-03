package tunnel

import "sync/atomic"

// Both series retain the existing stream-byte accounting. QUIC packet overhead
// and retransmissions remain exclusively in QUICSnapshot packet counters.
type transportTraffic struct {
	tcpTX, tcpRX, quicTX, quicRX atomic.Uint64
}
type TransportBytesSnapshot struct {
	TX uint64 `json:"tx_bytes"`
	RX uint64 `json:"rx_bytes"`
}
type TransportTrafficSnapshot struct {
	TCP  TransportBytesSnapshot `json:"tcp"`
	QUIC TransportBytesSnapshot `json:"quic"`
}

func (t *transportTraffic) add(quic, tx bool, n int) {
	counter := &t.tcpRX
	if quic {
		counter = &t.quicRX
	}
	if tx {
		if quic {
			counter = &t.quicTX
		} else {
			counter = &t.tcpTX
		}
	}
	counter.Add(uint64(n))
}
func (t *transportTraffic) snapshot() TransportTrafficSnapshot {
	return TransportTrafficSnapshot{
		TCP:  TransportBytesSnapshot{TX: t.tcpTX.Load(), RX: t.tcpRX.Load()},
		QUIC: TransportBytesSnapshot{TX: t.quicTX.Load(), RX: t.quicRX.Load()},
	}
}
