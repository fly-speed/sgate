package master_gin

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	master "github.com/acl-dev/go-service"
	"github.com/gin-gonic/gin"
)

type ServerConfig struct {
	Addresses         string
	TLSConfig         *tls.Config
	Handler           http.Handler
	WrapListener      func(net.Listener) net.Listener
	OnStop            func()
	ReadHeaderTimeout time.Duration
	IdleTimeout       time.Duration
	MaxHeaderBytes    int
}
type workerKey struct{}

// Handler resolves the immutable worker from the request, never a newer
// generation's proxy. Hijacked streams therefore cannot cross generations.
func (s *GinService) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		worker, _ := r.Context().Value(workerKey{}).(*Worker)
		if worker == nil || worker.config.Handler == nil {
			http.NotFound(w, r)
			return
		}
		worker.config.Handler.ServeHTTP(w, r)
	})
}

type Worker struct {
	service     *GinService
	config      ServerConfig
	mu          sync.Mutex
	started     bool
	closed      bool
	transports  []net.Listener
	listeners   []net.Listener
	servers     []*http.Server
	connections map[*ownedConn]struct{}
	handlers    sync.WaitGroup
	serve       sync.WaitGroup
	closeOnce   sync.Once
	done        chan struct{}
	err         error
}

func (s *GinService) Configure(config ServerConfig) (*Worker, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.configureLocked(config)
}
func (s *GinService) configureLocked(config ServerConfig) (*Worker, error) {
	if s.closed || s.ctx.Err() != nil {
		return nil, net.ErrClosed
	}
	if s.worker != nil {
		s.worker.mu.Lock()
		closed := s.worker.closed
		s.worker.mu.Unlock()
		if !closed {
			return nil, errors.New("previous HTTP worker still running")
		}
	}
	if config.Addresses == "" {
		config.Addresses = s.addresses
	}
	if s.Alone && (config.Addresses != s.addresses || len(s.pools) == 0) {
		for _, pool := range s.pools {
			pool.Close()
		}
		s.pools = nil
		listeners, e := listen(config.Addresses)
		if e != nil {
			s.addresses = ""
			return nil, e
		}
		engines := s.Servers
		s.Servers = nil
		s.install(listeners)
		for i, v := range s.Servers {
			if len(engines) > 0 {
				v.Engine = engines[min(i, len(engines)-1)].Engine
			}
		}
		s.addresses = config.Addresses
	}
	w := &Worker{service: s, config: config, connections: make(map[*ownedConn]struct{}), done: make(chan struct{})}
	for i, pool := range s.pools {
		if err := w.addListener(pool, s.Servers[i].Engine); err != nil {
			for _, listener := range w.listeners {
				listener.Close()
			}
			return nil, err
		}
	}

	s.worker = w
	if s.running {
		w.start()
	}
	return w, nil
}
func (w *Worker) PrimaryListener() net.Listener { return w.listeners[0] }
func (w *Worker) Done() <-chan struct{}         { return w.done }
func (w *Worker) Wait() error {
	<-w.done
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}
func (w *Worker) start() {
	w.mu.Lock()
	if w.started || w.closed {
		w.mu.Unlock()
		return
	}
	w.started = true
	w.serve.Add(len(w.servers))
	w.mu.Unlock()
	for i, server := range w.servers {
		go func(server *http.Server, transport net.Listener) {
			defer w.serve.Done()
			e := server.Serve(transport)
			if e != nil && !errors.Is(e, http.ErrServerClosed) && !errors.Is(e, net.ErrClosed) {
				w.mu.Lock()
				w.err = e
				w.mu.Unlock()
				w.service.fail(e)
			}
		}(server, w.transports[i])
	}
}
func (w *Worker) Close() error {
	w.closeOnce.Do(func() {
		w.mu.Lock()
		w.closed = true
		connections := make([]*ownedConn, 0, len(w.connections))
		for c := range w.connections {
			connections = append(connections, c)
		}
		w.mu.Unlock()
		// Close the physical TCP connections before invoking protocol shutdown.
		// Otherwise a TLS close_notify or a handler blocked on writing can hold
		// the shutdown hook indefinitely, especially after a WebSocket hijack.
		for _, c := range connections {
			c.Close()
		}
		if w.config.OnStop != nil {
			w.config.OnStop()
		}
		for _, l := range w.listeners {
			l.Close()
		}
		for _, server := range w.servers {
			server.Close()
		}
		w.serve.Wait()
		w.handlers.Wait()
		close(w.done)
	})
	return w.Wait()
}

type ownedListener struct {
	net.Listener
	worker *Worker
}

func (l *ownedListener) Accept() (net.Conn, error) {
	c, e := l.Listener.Accept()
	if e != nil {
		return nil, e
	}
	w := l.worker
	owned := &ownedConn{Conn: c, worker: w}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		c.Close()
		return nil, net.ErrClosed
	}
	w.connections[owned] = struct{}{}
	master.ConnCountInc()
	w.service.active.Add(1)
	w.mu.Unlock()
	if w.service.AcceptHandler != nil {
		w.service.AcceptHandler(owned)
	}
	return owned, nil
}

type ownedConn struct {
	net.Conn
	worker *Worker
	once   sync.Once
}

func (c *ownedConn) Close() error {
	e := c.Conn.Close()
	c.once.Do(func() {
		w := c.worker
		w.mu.Lock()
		delete(w.connections, c)
		w.mu.Unlock()
		master.ConnCountDec()
		w.service.active.Add(-1)
		if w.service.CloseHandler != nil {
			w.service.CloseHandler(c)
		}
	})
	return e
}

// 每个监听共用同一个 Worker；请求闭包只引用这一代的处理器和等待组。
func (w *Worker) addListener(pool *listenerPool, engine *gin.Engine) error {
	config := w.config
	lease, e := pool.Lease()
	if e != nil {
		return e
	}
	w.listeners = append(w.listeners, lease)
	var transport net.Listener = lease
	if config.WrapListener != nil {
		transport = config.WrapListener(transport)
	}
	transport = &ownedListener{Listener: transport, worker: w}
	if config.TLSConfig != nil {
		transport = tls.NewListener(transport, config.TLSConfig.Clone())
	}
	handler := http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		if w.closed {
			w.mu.Unlock()
			http.Error(rw, "service stopping", http.StatusServiceUnavailable)
			return
		}
		w.handlers.Add(1)
		w.mu.Unlock()
		defer w.handlers.Done()
		engine.ServeHTTP(rw, r.WithContext(context.WithValue(r.Context(), workerKey{}, w)))
	})
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: config.ReadHeaderTimeout,
		IdleTimeout:       config.IdleTimeout,
		MaxHeaderBytes:    config.MaxHeaderBytes,
	}
	w.servers = append(w.servers, server)
	// Serve uses TLS-wrapped transports while public Listener describes the
	// disposable lease. Closing a lease never closes framework-owned sockets.
	w.transports = append(w.transports, transport)
	return nil
}
