package app

import (
	"context"
	"errors"
	"log"
	"net"
	"net/url"
	"sync"

	"sgate/internal/tunnel"
)

type proxyWorker struct {
	route    clientProxy
	client   *tunnel.Client
	listener *proxyListener
	cancel   context.CancelFunc
	done     chan struct{}
}
type clientProxyGroup struct {
	mu       sync.RWMutex
	ctx      context.Context
	cancel   context.CancelFunc
	listener net.Listener
	routes   map[string]*proxyWorker
	err      error
}

func newClientProxyGroup(ctx context.Context, listener net.Listener, routes []clientProxy) (*clientProxyGroup, error) {
	ctx, cancel := context.WithCancel(ctx)
	g := &clientProxyGroup{ctx: ctx, cancel: cancel, listener: listener, routes: map[string]*proxyWorker{}}
	if err := g.replace(routes, nil); err != nil {
		cancel()
		return nil, err
	}
	return g, nil
}

// Admission is paused only during the listener/map commit. Bind failures leave
// old routes and all their sessions intact; unchanged workers are reused.
func (g *clientProxyGroup) replace(routes []clientProxy, update func() error) error {
	g.mu.Lock()
	if g.ctx.Err() != nil {
		g.mu.Unlock()
		return net.ErrClosed
	}
	next := make(map[string]*proxyWorker, len(routes))
	var added, removed []*proxyWorker
	for _, route := range routes {
		old := g.routes[route.port]
		if old != nil && old.route.Listen == route.Listen && old.route.config.Username == route.config.Username && old.route.config.Password == route.config.Password && old.route.config.AllowReverse == route.config.AllowReverse && proxyRouteEndpoint(old.route.config) == proxyRouteEndpoint(route.config) {
			next[route.port] = old
			continue
		}
		client, err := tunnel.NewClient(route.config)
		if err != nil {
			g.mu.Unlock()
			return err
		}
		size := route.config.Options.MaxStreams
		if size <= 0 {
			size = 128
		}
		address, _ := net.ResolveTCPAddr("tcp", route.Listen)
		w := &proxyWorker{route: route, client: client, listener: &proxyListener{incoming: make(chan net.Conn, size), done: make(chan struct{}), address: address}, done: make(chan struct{})}
		next[route.port] = w
		added = append(added, w)
	}
	if update != nil {
		if err := update(); err != nil {
			g.mu.Unlock()
			return err
		}
	}
	for port, old := range g.routes {
		if next[port] != old {
			removed = append(removed, old)
		}
	}
	g.routes = next
	for _, w := range added {
		ctx, cancel := context.WithCancel(g.ctx)
		w.cancel = cancel
		go func(w *proxyWorker) {
			defer close(w.done)
			err := w.client.Run(ctx, w.listener)
			if ctx.Err() == nil {
				g.fail(err)
			}
		}(w)
	}
	g.mu.Unlock()
	stopProxyWorkers(removed)
	return nil
}
func stopProxyWorkers(workers []*proxyWorker) {
	for _, w := range workers {
		// Run owns this listener. Closing it before Run handles cancellation
		// would race the authenticated revocation notification.
		w.cancel()
	}
	for _, w := range workers {
		<-w.done
		for len(w.listener.incoming) > 0 {
			(<-w.listener.incoming).Close()
		}
	}
}
func (g *clientProxyGroup) fail(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ctx.Err() == nil {
		if err == nil {
			err = errors.New("proxy worker stopped unexpectedly")
		}
		g.err = err
		g.cancel()
	}
}
func (g *clientProxyGroup) run() error {
	done := make(chan struct{})
	go func() {
		defer close(done)
		err := g.dispatch()
		if g.ctx.Err() == nil {
			g.fail(err)
		}
	}()
	<-g.ctx.Done()
	g.listener.Close()
	<-done
	g.mu.Lock()
	workers := make([]*proxyWorker, 0, len(g.routes))
	for _, w := range g.routes {
		workers = append(workers, w)
	}
	g.routes = map[string]*proxyWorker{}
	g.mu.Unlock()
	stopProxyWorkers(workers)
	return g.err
}
func (g *clientProxyGroup) dispatch() error {
	for {
		conn, err := g.listener.Accept()
		if err != nil {
			return err
		}
		_, port, err := net.SplitHostPort(conn.LocalAddr().String())
		g.mu.RLock()
		w := g.routes[port]
		if err != nil || w == nil {
			g.mu.RUnlock()
			conn.Close()
			continue
		}
		select {
		case <-g.ctx.Done():
			conn.Close()
		case <-w.listener.done:
			conn.Close()
		case w.listener.incoming <- conn:
		default:
			log.Printf("TCP connection rejected: listen=%q peer=%q reason=proxy accept queue full", conn.LocalAddr(), conn.RemoteAddr())
			conn.Close()
		}
		g.mu.RUnlock()
	}
}

func proxyRouteEndpoint(cfg tunnel.ClientConfig) string {
	u, _ := url.Parse(cfg.URL)
	q := u.Query()
	if cfg.OmitAddr {
		q.Del("addr")
	} else if cfg.Addr != "" {
		q.Set("addr", cfg.Addr)
	}
	u.RawQuery = q.Encode()
	return u.String()
}
