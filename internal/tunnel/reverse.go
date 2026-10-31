package tunnel

import (
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"sync/atomic"
	"time"
)

const reverseQuery = "sgate-reverse"
const reverseStreamBase uint64 = 1 << 63
const maxReverseMappings = 256
const maxReverseListeners = 4096

// ReverseMapping exposes Listen on the server and dials Target from the client.
// Listen requires a literal IP to make interface exposure explicit.
type ReverseMapping struct {
	Listen string `json:"listen"`
	Target string `json:"target"`
}

type reverseListener struct {
	net.Listener
	address string // Configured policy key; do not infer it from OS wildcard normalization.
	owner   atomic.Pointer[Server]
}

func validateReverseMappings(accounts []Account) error {
	seen := make(map[string]bool)
	total := 0
	for _, account := range accounts {
		if len(account.ReverseMappings) > maxReverseMappings {
			return errors.New("at most 256 reverse mappings per account")
		}
		for _, mapping := range account.ReverseMappings {
			host, port, err := net.SplitHostPort(mapping.Listen)
			if err != nil || net.ParseIP(host) == nil || validateSelectableTarget(mapping.Listen) != nil {
				return errors.New("reverse listen must be a literal IP:port with port 1 to 65535")
			}
			if validateSelectableTarget(mapping.Target) != nil {
				return errors.New("reverse target must be a valid host:port with port 1 to 65535")
			}
			key := net.JoinHostPort(net.ParseIP(host).String(), port)
			n, _ := strconv.Atoi(port)
			if key != mapping.Listen || strconv.Itoa(n) != port {
				return errors.New("reverse listen must use canonical IP:port notation")
			}
			if seen[key] {
				return errors.New("duplicate reverse listen address")
			}
			seen[key] = true
			total++
		}
	}
	if total > maxReverseListeners {
		return errors.New("at most 4096 reverse mappings per server")
	}
	return nil
}

func sameReverseMappings(a, b []ReverseMapping) bool { return slices.Equal(a, b) }

// StartReverse starts configured listeners. Configuration validation alone never
// binds ports. Repeated calls are safe; account updates reconcile these listeners.
func (s *Server) StartReverse() error { return s.StartReverseFrom(nil) }

// StartReverseFrom transfers matching listeners during shared HTTP replacement.
// New binds complete before ownership changes, so a failure leaves the old
// server and its listeners intact. Callers serialize replacement with shutdown.
func (s *Server) StartReverseFrom(previous *Server) error {
	if previous == s {
		return s.StartReverse()
	}
	if previous != nil {
		previous.mu.Lock()
		defer previous.mu.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("server is closed")
	}
	if err := s.reconcileReverseLocked(s.accounts, previous); err != nil {
		return err
	}
	s.reverseStarted = true
	return nil
}

// All policy changes and accepts use Server.mu. A provisional listener does not
// accept until the complete new policy has been installed under that lock.
func (s *Server) reconcileReverseLocked(accounts map[string]accountVerifier, previous *Server) error {
	next := make(map[string]*reverseListener)
	created := make([]*reverseListener, 0)
	for _, account := range accounts {
		if account.disabled {
			continue
		}
		for _, mapping := range account.reverse {
			listener := s.reverseListeners[mapping.Listen]
			if listener == nil && previous != nil {
				listener = previous.reverseListeners[mapping.Listen]
			}
			if listener == nil {
				host, _, _ := net.SplitHostPort(mapping.Listen)
				network := "tcp6"
				if net.ParseIP(host).To4() != nil {
					network = "tcp4"
				}
				listen := s.cfg.ReverseListen
				if listen == nil {
					listen = net.Listen
				}
				l, err := listen(network, mapping.Listen)
				if err != nil {
					for _, pending := range created {
						pending.Close()
					}
					return fmt.Errorf("cannot bind reverse listener %s: %w", mapping.Listen, err)
				}
				listener = &reverseListener{Listener: l, address: mapping.Listen}
				created = append(created, listener)
			}
			next[mapping.Listen] = listener
		}
	}
	for address, listener := range s.reverseListeners {
		if next[address] != listener {
			listener.Close()
		}
	}
	for address, listener := range next {
		listener.owner.Store(s)
		if previous != nil && previous.reverseListeners[address] == listener {
			delete(previous.reverseListeners, address)
		}
	}
	s.reverseListeners = next
	for _, listener := range created {
		go listener.run()
	}
	return nil
}

func (s *Server) closeReverseLocked() {
	for _, listener := range s.reverseListeners {
		listener.Close()
	}
	s.reverseListeners = nil
}

func (l *reverseListener) run() {
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		owner := l.owner.Load()
		if owner == nil {
			conn.Close()
			continue
		}
		owner.acceptReverse(l, conn)
	}
}

func (s *Server) acceptReverse(listener *reverseListener, conn net.Conn) {
	s.mu.Lock()
	address := listener.address
	if s.closed || s.reverseListeners[address] != listener {
		s.mu.Unlock()
		conn.Close()
		return
	}
	var username, target string
	for name, account := range s.accounts {
		if account.disabled {
			continue
		}
		for _, mapping := range account.reverse {
			if mapping.Listen == address {
				username, target = name, mapping.Target
				break
			}
		}
	}
	// Multiple forward mappings may create multiple sessions for the same account.
	// Select deterministically, and never route to a different account or an old
	// client that did not advertise support in its authenticated handshake.
	var chosen *session
	var chosenID string
	for id, entry := range s.sessions {
		if username != "" && entry.username == username && entry.reverse && entry.ready && entry.inUse && (chosen == nil || id < chosenID) {
			chosen, chosenID = entry.session, id
		}
	}
	if chosen == nil {
		s.mu.Unlock()
		conn.Close()
		return
	}
	// Admission is bounded and atomic with revocation; a full queue rejects
	// the new connection without blocking the listener or account updates.
	err := chosen.reserveReverse(conn, target)
	s.mu.Unlock()
	if err != nil {
		conn.Close()
	}
}

func (s *session) reserveReverse(conn net.Conn, target string) error {
	s.mu.Lock()
	if s.closed || s.active == nil || len(s.streams) >= s.opts.MaxStreams || s.nextStream >= reverseStreamBase-1 || s.pendingBytes+frameHeader+len(target) > s.opts.BufferBytes {
		s.mu.Unlock()
		return errors.New("reverse session unavailable or stream limit reached")
	}
	s.nextStream++
	id := reverseStreamBase + s.nextStream
	st := &stream{conn: conn}
	if len(s.streams) == 0 {
		s.lastBusiness = time.Time{}
	}
	s.streams[id] = st
	s.next++
	s.pending = append(s.pending, frame{op: opReverseOpen, seq: s.next, stream: id, data: []byte(target)})
	s.pendingBytes += frameHeader + len(target)
	s.signalLocked()
	s.mu.Unlock()
	go s.readStream(id, st)
	return nil
}
