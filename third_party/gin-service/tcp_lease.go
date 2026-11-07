package master_gin

import (
	"errors"
	"net"
	"sync"
)

// LeaseTCP reserves inherited sockets for a raw TCP protocol before Configure.
// A wildcard address can correspond to several interface sockets in acl_master.
// Closing the lease releases admission, while the framework retains its sockets.
func (s *GinService) LeaseTCP(address string) (net.Listener, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.ctx.Err() != nil {
		return nil, net.ErrClosed
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, errors.New("raw TCP lease requires a literal IP")
	}
	var pools []*listenerPool
	for _, pool := range s.pools {
		h, p, _ := net.SplitHostPort(pool.listeners[0].Addr().String())
		bound := net.ParseIP(h)
		if p == port && bound != nil && (ip.Equal(bound) || (ip.IsUnspecified() && (ip.To4() == nil) == (bound.To4() == nil))) {
			pools = append(pools, pool)
		}
	}
	if len(pools) == 0 {
		return nil, errors.New("reverse listener missing from master_service: " + address)
	}
	resolved, err := net.ResolveTCPAddr("tcp", address)
	if err != nil {
		return nil, err
	}
	l := &tcpLease{incoming: make(chan net.Conn), done: make(chan struct{}), address: resolved}
	for _, pool := range pools {
		lease, err := pool.Lease()
		if err != nil {
			l.Close()
			return nil, err
		}
		l.listeners = append(l.listeners, lease)
	}
	for _, pool := range pools {
		pool.raw = true
	}
	if !s.Alone {
		// acl_master can expand one raw service port across multiple interfaces.
		// Reserve the entire port from HTTP, while only leasing the configured
		// bind addresses. Unleased interfaces close incoming connections.
		for _, pool := range s.pools {
			_, boundPort, _ := net.SplitHostPort(pool.listeners[0].Addr().String())
			if boundPort == port {
				pool.raw = true
			}
		}
	}
	for _, lease := range l.listeners {
		l.wg.Add(1)
		go func(lease net.Listener) {
			defer l.wg.Done()
			for {
				c, err := lease.Accept()
				if err != nil {
					return
				}
				select {
				case l.incoming <- c:
				case <-l.done:
					c.Close()
					return
				}
			}
		}(lease)
	}
	return l, nil
}

type tcpLease struct {
	listeners []net.Listener
	incoming  chan net.Conn
	done      chan struct{}
	address   net.Addr
	once      sync.Once
	wg        sync.WaitGroup
}

func (l *tcpLease) Accept() (net.Conn, error) {
	select {
	case <-l.done:
		return nil, net.ErrClosed
	case c := <-l.incoming:
		select {
		case <-l.done:
			c.Close()
			return nil, net.ErrClosed
		default:
			return c, nil
		}
	}
}
func (l *tcpLease) Addr() net.Addr { return l.address }
func (l *tcpLease) Close() error {
	l.once.Do(func() {
		close(l.done)
		for _, lease := range l.listeners {
			lease.Close()
		}
		l.wg.Wait()
	})
	return nil
}
