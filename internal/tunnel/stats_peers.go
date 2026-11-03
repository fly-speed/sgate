package tunnel

import (
	"net"
	"sort"
	"sync/atomic"
	"time"
)

const maxTrafficPeers = 1024
const trafficPeerRetention = time.Minute
const otherTrafficPeers = "__other__"

type peerTraffic struct {
	transports                                       transportTraffic
	tx, rx, noiseTX, noiseRX, businessTX, businessRX atomic.Uint64
	active                                           int64 // guarded by Stats.peersMu
	idle                                             time.Time
}

type PeerTrafficSnapshot struct {
	Transports  TransportTrafficSnapshot `json:"transports"`
	IP          string                   `json:"ip"`
	TX          uint64                   `json:"tx_bytes"`
	RX          uint64                   `json:"rx_bytes"`
	NoiseTX     uint64                   `json:"noise_tx_bytes"`
	NoiseRX     uint64                   `json:"noise_rx_bytes"`
	BusinessTX  uint64                   `json:"business_tx_bytes"`
	BusinessRX  uint64                   `json:"business_rx_bytes"`
	Connections int64                    `json:"connections"`
}

func trafficPeerIP(addr net.Addr) string {
	if addr == nil {
		return "__unknown__"
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return "__unknown__"
	}
	// Canonicalize IP literals, without reverse DNS or trusting HTTP headers.
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return host
}

// Retain closed peers for one chart window; cap cardinality so changing source
// addresses cannot grow monitoring memory indefinitely. Active peers are never evicted.
func (s *Stats) prunePeersLocked(now time.Time) {
	for ip, p := range s.peers {
		if p.active == 0 && now.Sub(p.idle) > trafficPeerRetention {
			delete(s.peers, ip)
		}
	}
}
func (s *Stats) openPeer(addr net.Addr) *peerTraffic {
	s.peersMu.Lock()
	defer s.peersMu.Unlock()
	if s.peers == nil {
		s.peers = make(map[string]*peerTraffic)
	}
	s.prunePeersLocked(time.Now())
	ip := trafficPeerIP(addr)
	p := s.peers[ip]
	if p == nil {
		if len(s.peers) >= maxTrafficPeers {
			ip = otherTrafficPeers
			p = s.peers[ip]
		}
		if p == nil {
			p = &peerTraffic{}
			s.peers[ip] = p
		}
	}
	p.active++
	return p
}
func (s *Stats) peerSnapshots() []PeerTrafficSnapshot {
	s.peersMu.Lock()
	defer s.peersMu.Unlock()
	s.prunePeersLocked(time.Now())
	peers := make([]PeerTrafficSnapshot, 0, len(s.peers))
	for ip, p := range s.peers {
		peers = append(peers, PeerTrafficSnapshot{
			IP: ip, Connections: p.active,
			Transports: p.transports.snapshot(),
			TX:         p.tx.Load(), RX: p.rx.Load(),
			NoiseTX: p.noiseTX.Load(), NoiseRX: p.noiseRX.Load(),
			BusinessTX: p.businessTX.Load(), BusinessRX: p.businessRX.Load(),
		})
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].IP < peers[j].IP })
	return peers
}

// Payload counts share the same peer bucket as the underlying TCP bytes.
func (s *Stats) peerPayload(conn net.Conn, kind string, size int) {
	var p *peerTraffic
	// Follow TLS to the exact TCP counter, preserving attribution even when
	// multiple overflow peers share a bucket or reconnect during a snapshot.
	for conn != nil {
		if counted, ok := conn.(*trafficConn); ok {
			if counted.stats == s {
				p = counted.peer
			}
			break
		}
		wrapped, ok := conn.(interface{ NetConn() net.Conn })
		if !ok {
			break
		}
		conn = wrapped.NetConn()
	}
	if p == nil {
		return
	}

	switch kind {
	case "noiseTX":
		p.noiseTX.Add(uint64(size))
	case "noiseRX":
		p.noiseRX.Add(uint64(size))
	case "businessTX":
		p.businessTX.Add(uint64(size))
	case "businessRX":
		p.businessRX.Add(uint64(size))
	}
}
