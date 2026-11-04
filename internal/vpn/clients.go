package vpn

import (
	"net"
	"net/netip"
	"sort"
)

// ClientSnapshot contains only authenticated session metadata. PublicIP is the
// source address observed by the server, which may be a NAT or relay address.
type ClientSnapshot struct {
	PublicIP      string `json:"public_ip"`
	RemoteAddress string `json:"remote_address"`
	VPNIP         string `json:"vpn_ip"`
	Transport     string `json:"transport"`
}

func (s *Server) Clients() []ClientSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	clients := make([]ClientSnapshot, 0, len(s.peers))
	for ip, p := range s.peers {
		if p.conn.Context().Err() != nil {
			continue
		}
		remote := p.conn.RemoteAddr().String()
		host, _, err := net.SplitHostPort(remote)
		if err != nil {
			host = remote
		}
		if parsed, e := netip.ParseAddr(host); e == nil {
			host = parsed.Unmap().String()
		}
		transport := "quic"
		if _, ok := p.conn.(*udpConnection); ok {
			transport = "udp"
		}
		if conn, ok := p.conn.(*recordConnection); ok {
			transport = "tcp"
			if _, ok := conn.raw.(*websocketConn); ok {
				transport = "wss"
			}
		}
		clients = append(clients, ClientSnapshot{PublicIP: host, RemoteAddress: remote, VPNIP: ip.String(), Transport: transport})
	}
	sort.Slice(clients, func(i, j int) bool { return clients[i].VPNIP < clients[j].VPNIP })
	return clients
}
