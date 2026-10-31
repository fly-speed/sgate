package master

import (
	"errors"
	"net"
	"strings"
)

func tcpAddressParts(addresses string) []string {
	return strings.FieldsFunc(addresses, func(r rune) bool { return r == ',' || r == ';' })
}

// UpdateAddresses changes standalone admission without replacing the worker or
// accepted connections. Bind every addition first; a failure leaves all old
// listeners intact. Managed sockets must be changed by acl_master itself.
func (s *TcpService) UpdateAddresses(addresses string) error {
	life := s.lifecycle
	life.op.Lock()
	defer life.op.Unlock()
	life.mu.Lock()
	if life.closed || life.ctx.Err() != nil {
		life.mu.Unlock()
		return net.ErrClosed
	}
	if life.managed {
		life.mu.Unlock()
		return errors.New("managed TCP addresses require framework restart")
	}
	sockets := life.sockets
	if sockets == nil || life.worker == nil {
		life.mu.Unlock()
		return errors.New("TCP worker is not active")
	}
	old := append([]net.Listener(nil), sockets.listeners...)
	oldAddresses := tcpAddressParts(life.addresses)
	life.mu.Unlock()
	// A wildcard configured as 0.0.0.0 may report [::] after net.Listen.
	// Keep configured identity as well as the physical address for port-zero APIs.
	byAddress := map[string]net.Listener{}
	for n, l := range old {
		byAddress[l.Addr().String()] = l
		if n < len(oldAddresses) {
			if a, err := net.ResolveTCPAddr("tcp", strings.ReplaceAll(strings.TrimSpace(oldAddresses[n]), "|", ":")); err == nil {
				byAddress[a.String()] = l
				if a.Port == 0 {
					if actual, err := net.ResolveTCPAddr("tcp", l.Addr().String()); err == nil {
						a.Port = actual.Port
						byAddress[a.String()] = l
					}
				}
			}
		}
	}
	var next, added []net.Listener
	used := map[net.Listener]bool{}
	seen := map[string]bool{}
	rollback := func() {
		for _, l := range added {
			l.Close()
		}
	}
	for _, raw := range tcpAddressParts(addresses) {
		a, err := net.ResolveTCPAddr("tcp", strings.ReplaceAll(strings.TrimSpace(raw), "|", ":"))
		if err != nil {
			rollback()
			return err
		}
		key := a.String()
		if seen[key] {
			rollback()
			return errors.New("duplicate TCP listening address")
		}
		seen[key] = true
		l := byAddress[key]
		if l != nil && used[l] {
			rollback()
			return errors.New("duplicate physical TCP listener")
		}
		if l == nil {
			l, err = net.Listen("tcp", key)
			if err != nil {
				rollback()
				return err
			}
			added = append(added, l)
		}
		used[l] = true
		next = append(next, l)
	}
	if len(next) == 0 {
		rollback()
		return errors.New("TCP listening address required")
	}
	life.mu.Lock()
	sockets.listeners, life.addresses = next, addresses
	life.worker.mu.Lock()
	life.worker.address = next[0].Addr()
	life.worker.mu.Unlock()
	if life.running {
		s.startListenersLocked(sockets, added)
	}
	var removed []net.Listener
	var stopped []chan struct{}
	for _, l := range old {
		if !used[l] {
			removed = append(removed, l)
			if done := sockets.done[l]; done != nil {
				stopped = append(stopped, done)
				delete(sockets.done, l)
			}
		}
	}
	life.mu.Unlock()
	for _, l := range removed {
		l.Close()
	}
	for _, done := range stopped {
		<-done
	}
	return nil
}
