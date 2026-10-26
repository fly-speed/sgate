package master

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
)

type TCPOption func(*tcpOptions)
type tcpOptions struct {
	ctx     context.Context
	managed bool
}

func WithTCPContext(ctx context.Context) TCPOption { return func(o *tcpOptions) { o.ctx = ctx } }
func WithTCPManaged(managed bool) TCPOption        { return func(o *tcpOptions) { o.managed = managed } }

type tcpListeners struct {
	listeners []net.Listener
	wg        sync.WaitGroup
	started   bool
	done      map[net.Listener]chan struct{}
}
type tcpLifecycle struct {
	op          sync.Mutex
	mu          sync.Mutex
	ctx         context.Context
	cancel      context.CancelFunc
	managed     bool
	closed      bool
	running     bool
	addresses   string
	sockets     *tcpListeners
	worker      *TCPWorker
	handlers    sync.WaitGroup
	state       net.Conn
	monitorDone chan struct{}
	err         error
}

// initTCP uses the template's configuration and inherited descriptors, while
// replacing the process-global Stop/Wait monitor with an owned context.
func initTCP(addresses string, options ...TCPOption) (*TcpService, error) {
	o := tcpOptions{ctx: context.Background()}
	for _, apply := range options {
		apply(&o)
	}
	if o.managed {
		if SocketCount < 1 || SocketCount > 256 || Chroot || Unprivileged {
			return nil, errors.New("invalid managed TCP descriptor count or privilege options")
		}
		if ServiceType != "sock" && ServiceType != "inet" && ServiceType != "unix" {
			return nil, errors.New("unsupported managed TCP service type")
		}
		if _, err := closeInheritedSockets(SocketCount); err != nil {
			return nil, err
		}
	}
	Prepare()
	if o.managed && (AppConf.GetInt("master_maxproc") != 1 || AppConf.GetInt("master_prefork") != 1 || AppReusePort || AppConf.GetBool("master_status_notify")) {
		return nil, errors.New("go-service requires maxproc=prefork=1, reuseport=no, status_notify=0")
	}
	ctx, cancel := context.WithCancel(o.ctx)
	life := &tcpLifecycle{ctx: ctx, cancel: cancel, managed: o.managed, addresses: addresses, monitorDone: make(chan struct{})}
	service := &TcpService{lifecycle: life}
	var listeners []net.Listener
	var err error
	if o.managed {
		listeners, err = GetListeners()
	} else {
		listeners, err = listenTCP(addresses)
	}
	if err == nil && o.managed && len(listeners) != SocketCount {
		err = errors.New("incomplete managed TCP listeners")
	}
	if err != nil {
		for _, l := range listeners {
			l.Close()
		}
		cancel()
		return nil, err
	}
	life.sockets = &tcpListeners{listeners: listeners}
	if o.managed {
		f := os.NewFile(5, "go-service acl_master status")
		life.state, err = net.FileConn(f)
		f.Close()
		if err != nil {
			close(life.monitorDone)
			service.Close()
			return nil, fmt.Errorf("framework status: %w", err)
		}
		go func() { defer close(life.monitorDone); var b [1]byte; _, _ = life.state.Read(b[:]); cancel() }()
	} else {
		close(life.monitorDone)
	}
	log.Printf("go-service Init: standalone=%t listeners=%v", !o.managed, service.Addresses())
	return service, nil
}

func listenTCP(addresses string) ([]net.Listener, error) {
	var result []net.Listener
	for _, address := range strings.FieldsFunc(addresses, func(r rune) bool { return r == ';' || r == ',' }) {
		l, err := net.Listen("tcp", strings.ReplaceAll(strings.TrimSpace(address), "|", ":"))
		if err != nil {
			for _, old := range result {
				old.Close()
			}
			return nil, err
		}
		result = append(result, l)
	}
	if len(result) == 0 {
		return nil, errors.New("TCP listening address required")
	}
	return result, nil
}
func (s *TcpService) Context() context.Context { return s.lifecycle.ctx }
func (s *TcpService) Managed() bool            { return s.lifecycle.managed }
func (s *TcpService) Addresses() []string {
	life := s.lifecycle
	life.mu.Lock()
	defer life.mu.Unlock()
	var addresses []string
	if life.sockets != nil {
		for _, l := range life.sockets.listeners {
			addresses = append(addresses, l.Addr().String())
		}
	}
	return addresses
}
func (s *TcpService) Err() error {
	life := s.lifecycle
	life.mu.Lock()
	defer life.mu.Unlock()
	return life.err
}
func (s *TcpService) runLifecycle() {
	life := s.lifecycle
	life.op.Lock()
	life.mu.Lock()
	if !life.closed {
		life.running = true
		s.startAcceptLocked()
	}
	life.mu.Unlock()
	life.op.Unlock()
	log.Printf("go-service Run: managed=%t", life.managed)
	<-life.ctx.Done()
	s.Close()
}

// startAcceptLocked starts each physical accept loop exactly once. It is called
// with mu held; handler additions finish before Close waits on handlers.
func (s *TcpService) startAcceptLocked() {
	life := s.lifecycle
	sockets := life.sockets
	if sockets == nil || sockets.started {
		return
	}
	sockets.started = true
	s.startListenersLocked(sockets, sockets.listeners)
}

func (s *TcpService) startListenersLocked(sockets *tcpListeners, listeners []net.Listener) {
	life := s.lifecycle
	if sockets.done == nil {
		sockets.done = make(map[net.Listener]chan struct{})
	}
	for _, l := range listeners {
		done := make(chan struct{})
		sockets.done[l] = done
		sockets.wg.Add(1)
		go func(l net.Listener) {
			defer sockets.wg.Done()
			defer close(done)
			for {
				conn, err := l.Accept()
				if err != nil {
					life.mu.Lock()
					expected := life.closed || life.sockets != sockets || !slices.Contains(sockets.listeners, l)
					if !expected {
						life.err = err
						life.cancel()
					}
					life.mu.Unlock()
					return
				}
				life.handlers.Add(1)
				go func() { defer life.handlers.Done(); s.handleConn(conn) }()
			}
		}(l)
	}
}
func closeTCPListeners(sockets *tcpListeners) {
	if sockets == nil {
		return
	}
	for _, l := range sockets.listeners {
		l.Close()
	}
	sockets.wg.Wait()
}

// Configure creates a disposable admission worker. Managed physical listeners
// stay open; standalone workers can bind a different address on hot apply.
func (s *TcpService) Configure(addresses string) (*TCPWorker, error) {
	life := s.lifecycle
	life.op.Lock()
	defer life.op.Unlock()
	life.mu.Lock()
	if life.closed || life.ctx.Err() != nil {
		life.mu.Unlock()
		return nil, net.ErrClosed
	}
	if life.worker != nil {
		life.mu.Unlock()
		return nil, errors.New("previous TCP worker is active")
	}
	if life.managed && addresses != life.addresses {
		life.mu.Unlock()
		return nil, errors.New("managed TCP address requires process restart")
	}
	sockets := life.sockets
	life.mu.Unlock()
	if sockets != nil && addresses != life.addresses && !life.managed {
		life.mu.Lock()
		life.sockets = nil
		life.mu.Unlock()
		closeTCPListeners(sockets)
		sockets = nil
	}
	if sockets == nil {
		listeners, err := listenTCP(addresses)
		if err != nil {
			return nil, err
		}
		sockets = &tcpListeners{listeners: listeners}
	}
	life.mu.Lock()
	defer life.mu.Unlock()
	life.sockets, life.addresses = sockets, addresses
	worker := &TCPWorker{service: s, incoming: make(chan net.Conn), done: make(chan struct{}), conns: make(map[*tcpWorkerConn]struct{}), address: sockets.listeners[0].Addr()}
	life.worker = worker
	if life.running {
		s.startAcceptLocked()
	}
	return worker, nil
}

// AcceptConn is called by the application's AcceptHandler. The callback stays
// alive until its application stream closes, so framework connection counts
// describe real stream lifetime rather than only the handoff.
func (s *TcpService) AcceptConn(conn net.Conn) {
	life := s.lifecycle
	life.mu.Lock()
	worker := life.worker
	life.mu.Unlock()
	if worker == nil {
		return
	}
	worker.deliver(conn)
}
func (s *TcpService) Close() error {
	life := s.lifecycle
	if life == nil {
		return errors.New("Close requires the contextual TCP lifecycle")
	}
	life.cancel()
	life.op.Lock()
	life.mu.Lock()
	if life.closed {
		life.mu.Unlock()
		life.op.Unlock()
		return nil
	}
	life.closed = true
	worker, sockets := life.worker, life.sockets
	life.worker, life.sockets = nil, nil
	life.mu.Unlock()
	if worker != nil {
		worker.stop()
	}
	closeTCPListeners(sockets)
	life.handlers.Wait()
	if life.state != nil {
		life.state.Close()
	}
	<-life.monitorDone
	life.op.Unlock()
	return nil
}

type TCPWorker struct {
	service  *TcpService
	incoming chan net.Conn
	done     chan struct{}
	mu       sync.Mutex
	closed   bool
	conns    map[*tcpWorkerConn]struct{}
	address  net.Addr
}
type tcpWorkerConn struct {
	net.Conn
	worker *TCPWorker
	once   sync.Once
	done   chan struct{}
}

func (c *tcpWorkerConn) Close() error {
	var err error
	c.once.Do(func() {
		err = c.Conn.Close()
		c.worker.mu.Lock()
		delete(c.worker.conns, c)
		c.worker.mu.Unlock()
		close(c.done)
	})
	return err
}

// Preserve TCP half-close through the callback adapter. Full Close alone
// would discard a response when the application finishes its request first.
func (c *tcpWorkerConn) CloseWrite() error {
	if conn, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return conn.CloseWrite()
	}
	return errors.New("connection does not support CloseWrite")
}

func (w *TCPWorker) deliver(conn net.Conn) {
	c := &tcpWorkerConn{Conn: conn, worker: w, done: make(chan struct{})}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.conns[c] = struct{}{}
	w.mu.Unlock()
	defer c.Close()
	select {
	case w.incoming <- c:
	case <-w.done:
		return
	}
	<-c.done
}
func (w *TCPWorker) Accept() (net.Conn, error) {
	select {
	case <-w.done:
		return nil, net.ErrClosed
	case c := <-w.incoming:
		select {
		case <-w.done:
			c.Close()
			return nil, net.ErrClosed
		default:
			return c, nil
		}
	}
}
func (w *TCPWorker) Addr() net.Addr { w.mu.Lock(); defer w.mu.Unlock(); return w.address }
func (w *TCPWorker) stop() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	close(w.done)
	conns := make([]*tcpWorkerConn, 0, len(w.conns))
	for c := range w.conns {
		conns = append(conns, c)
	}
	w.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
}
func (w *TCPWorker) Close() error {
	life := w.service.lifecycle
	life.op.Lock()
	defer life.op.Unlock()
	life.mu.Lock()
	if life.worker != w {
		life.mu.Unlock()
		w.stop()
		return nil
	}
	life.worker = nil
	var sockets *tcpListeners
	if !life.managed {
		sockets, life.sockets = life.sockets, nil
	}
	life.mu.Unlock()
	w.stop()
	closeTCPListeners(sockets)
	return nil
}
