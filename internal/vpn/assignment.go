package vpn

import "net/netip"

const assignmentChallengePrefix = paddingChallengePrefix + "address-v1:"

func requestedIP(ip netip.Addr) string {
	if !ip.IsValid() {
		return ""
	}
	return ip.String()
}

// The caller holds s.mu through allocation and insertion into s.peers. Only
// authenticated sessions reserve addresses; probes never consume a lease.
func (s *Server) allocateIPLocked(id string) netip.Addr {
	for ip, peer := range s.peers {
		if peer.id == id {
			return ip
		}
	}
	for ip := s.cfg.Network.Addr().Next().Next(); validClientIP(s.cfg.Network, ip); ip = ip.Next() {
		if s.peers[ip] == nil {
			return ip
		}
	}
	return netip.Addr{}
}
