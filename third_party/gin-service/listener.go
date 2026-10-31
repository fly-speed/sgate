package master_gin

import (
	"context"
	"errors"
	"net"
	"sync"
)

type listenerPool struct {
	raw       bool // Dedicated TCP lease; never serve HTTP or TLS on this pool.
	mu        sync.Mutex
	listeners []net.Listener
	current   *listenerLease
	closed    bool
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}
type listenerLease struct {
	pool     *listenerPool
	incoming chan net.Conn
	done     chan struct{}
	once     sync.Once
}

func newListenerPool(listeners []net.Listener, cancel context.CancelFunc) *listenerPool {
	p := &listenerPool{listeners: listeners, cancel: cancel}
	p.wg.Add(len(listeners))
	for _, l := range listeners {
		go p.accept(l)
	}
	return p
}
func (p *listenerPool) accept(l net.Listener) {
	defer p.wg.Done()
	for {
		c, e := l.Accept()
		if e != nil {
			p.mu.Lock()
			closed := p.closed
			p.mu.Unlock()
			if !closed {
				p.cancel()
			}
			return
		}
		p.mu.Lock()
		current := p.current
		p.mu.Unlock()
		if current == nil {
			c.Close()
			continue
		}
		select {
		case current.incoming <- c:
		case <-current.done:
			c.Close()
		}
	}
}
func (p *listenerPool) Lease() (net.Listener, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, net.ErrClosed
	}
	if p.current != nil {
		return nil, errors.New("previous acl_master listener listenerLease is still active")
	}
	l := &listenerLease{pool: p, incoming: make(chan net.Conn), done: make(chan struct{})}
	p.current = l
	return l, nil
}
func (l *listenerLease) Accept() (net.Conn, error) {
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
func (l *listenerLease) Close() error {
	l.once.Do(func() {
		l.pool.mu.Lock()
		if l.pool.current == l {
			l.pool.current = nil
		}
		close(l.done)
		l.pool.mu.Unlock()
	})
	return nil
}
func (l *listenerLease) Addr() net.Addr { return l.pool.listeners[0].Addr() }
func (p *listenerPool) Addresses() []string {
	addresses := make([]string, 0, len(p.listeners))
	for _, l := range p.listeners {
		addresses = append(addresses, l.Addr().String())
	}
	return addresses
}
func (p *listenerPool) Close() {
	p.mu.Lock()
	p.closed = true
	current := p.current
	p.mu.Unlock()
	if current != nil {
		current.Close()
	}
	for _, l := range p.listeners {
		l.Close()
	}
	p.wg.Wait()
}
