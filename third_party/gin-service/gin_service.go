// Package master_gin provides Gin services in standalone and acl_master modes.
// Based on acl-dev/gin-service v1.0.2; see UPSTREAM.md and LICENSE.
package master_gin

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	master "github.com/acl-dev/go-service"
	"github.com/gin-gonic/gin"
)

var Version = "1.0.2-sgate-lifecycle"

type AcceptFunc func(net.Conn)
type CloseFunc func(net.Conn)
type GinServ struct {
	Listener net.Listener
	Engine   *gin.Engine
}
type Option func(*initOptions)
type initOptions struct {
	ctx     context.Context
	managed *bool
}

func WithContext(ctx context.Context) Option { return func(o *initOptions) { o.ctx = ctx } }
func WithManaged(managed bool) Option        { return func(o *initOptions) { o.managed = &managed } }

var prepareMu sync.Mutex

type GinService struct {
	Alone         bool
	Servers       []*GinServ
	AcceptHandler AcceptFunc
	CloseHandler  CloseFunc
	mu            sync.Mutex
	ctx           context.Context
	cancel        context.CancelFunc
	signalStop    func()
	addresses     string
	pools         []*listenerPool
	worker        *Worker
	running       bool
	closed        bool
	started       bool
	state         net.Conn
	monitorDone   chan struct{}
	closeOnce     sync.Once
	runDone       chan struct{}
	active        atomic.Int64
	err           error
}

// Init owns listener acquisition and the framework status channel. Both modes
// share this initialization and Run/Close lifecycle; no process-global Wait loop.
func Init(addresses string, options ...Option) (*GinService, error) {
	o := initOptions{}
	for _, apply := range options {
		apply(&o)
	}
	managed := !master.Alone && master.Configure != ""
	if o.managed != nil {
		managed = *o.managed
	}
	if managed {
		if err := prepareManagedDescriptors(); err != nil {
			return nil, err
		}
	}
	prepareMu.Lock()
	master.Prepare()
	prepareMu.Unlock()
	if managed {
		if master.AppConf.GetInt("master_maxproc") != 1 || master.AppConf.GetInt("master_prefork") != 1 ||
			master.AppReusePort ||
			master.AppConf.GetBool("master_status_notify") {
			return nil, errors.New("gin-service requires maxproc=prefork=1, reuseport=no, status_notify=0")
		}
	}
	parent := o.ctx
	stop := func() {}
	if parent == nil {
		parent, stop = signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	}
	ctx, cancel := context.WithCancel(parent)
	s := &GinService{
		Alone:       !managed,
		ctx:         ctx,
		cancel:      cancel,
		signalStop:  stop,
		addresses:   addresses,
		monitorDone: make(chan struct{}),
		runDone:     make(chan struct{}),
	}
	var listeners []net.Listener
	var e error
	if managed {
		listeners, e = master.GetListeners()
		if e == nil && len(listeners) != master.SocketCount {
			e = errors.New("incomplete inherited listeners")
		}
	} else {
		listeners, e = listen(addresses)
	}
	if e != nil {
		for _, l := range listeners {
			l.Close()
		}
		cancel()
		stop()
		return nil, e
	}
	s.install(listeners)
	if managed {
		f := os.NewFile(5, "gin-service acl_master status")
		s.state, e = net.FileConn(f)
		f.Close()
		if e != nil {
			s.Close()
			return nil, fmt.Errorf("framework status channel: %w", e)
		}
		go func() {
			defer close(s.monitorDone)
			var b [1]byte
			_, _ = s.state.Read(b[:])
			s.cancel()
		}()
	} else {
		close(s.monitorDone)
	}
	log.Printf("gin-service Init: standalone=%t listeners=%v", s.Alone, s.Addresses())
	return s, nil
}
func listen(addresses string) ([]net.Listener, error) {
	if addresses == "" {
		return nil, errors.New("listening addresses required")
	}
	var listeners []net.Listener
	for _, address := range strings.FieldsFunc(addresses, func(r rune) bool { return r == ';' || r == ',' }) {
		address = strings.ReplaceAll(strings.TrimSpace(address), "|", ":")
		l, e := net.Listen("tcp", address)
		if e != nil {
			for _, old := range listeners {
				old.Close()
			}
			return nil, e
		}
		listeners = append(listeners, l)
	}
	if len(listeners) == 0 {
		return nil, errors.New("listening addresses required")
	}
	return listeners, nil
}
func (s *GinService) install(listeners []net.Listener) {
	for _, l := range listeners {
		s.pools = append(s.pools, newListenerPool([]net.Listener{l}, s.cancel))
		engine := gin.New()
		engine.Use(gin.Recovery())
		engine.RedirectTrailingSlash = false
		engine.RedirectFixedPath = false
		s.Servers = append(s.Servers, &GinServ{Listener: l, Engine: engine})
	}
}
func (s *GinService) Context() context.Context { return s.ctx }
func (s *GinService) Addresses() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, p := range s.pools {
		out = append(out, p.Addresses()...)
	}
	return out
}
func (s *GinService) ActiveConnections() int64 { return s.active.Load() }

// Run starts the configured HTTP/TLS worker, supervises the process lifetime,
// and joins its HTTP handlers, listeners and status monitor before returning.
func (s *GinService) Run() {
	s.mu.Lock()
	if s.started || s.closed {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.running = true
	if s.worker == nil {
		w, e := s.configureLocked(ServerConfig{Addresses: s.addresses})
		if e != nil {
			s.err = e
			s.mu.Unlock()
			s.Close()
			close(s.runDone)
			return
		}
		s.worker = w
	}
	s.worker.start()
	s.mu.Unlock()
	log.Printf("gin-service Run: standalone=%t", s.Alone)
	<-s.ctx.Done()
	s.Close()
	close(s.runDone)
	log.Printf("gin-service Run stopped")
}
func (s *GinService) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}
func (s *GinService) fail(e error) {
	if e == nil {
		return
	}
	s.mu.Lock()
	if s.err == nil {
		s.err = e
	}
	s.mu.Unlock()
	s.cancel()
}
func (s *GinService) Close() {
	s.closeOnce.Do(func() {
		s.cancel()
		s.signalStop()
		s.mu.Lock()
		s.closed = true
		w := s.worker
		pools := append([]*listenerPool(nil), s.pools...)
		s.mu.Unlock()
		if w != nil {
			w.Close()
		}
		for _, p := range pools {
			p.Close()
		}
		if s.state != nil {
			s.state.Close()
			<-s.monitorDone
		}
	})
}
func (s *GinService) GET(path string, h func(*gin.Context)) {
	for _, v := range s.Servers {
		v.Engine.GET(path, h)
	}
}
func (s *GinService) POST(path string, h func(*gin.Context)) {
	for _, v := range s.Servers {
		v.Engine.POST(path, h)
	}
}
func (s *GinService) HEAD(path string, h func(*gin.Context)) {
	for _, v := range s.Servers {
		v.Engine.HEAD(path, h)
	}
}
func (s *GinService) NoRoute(h func(*gin.Context)) {
	for _, v := range s.Servers {
		v.Engine.NoRoute(h)
	}
}

// 在 FileListener 接管监听前清理无主套接字，保留框架状态通道和监听 FD。
func prepareManagedDescriptors() error {
	if master.SocketCount < 1 || master.SocketCount > 256 {
		return errors.New("invalid acl_master listener count")
	}
	if master.Chroot || master.Unprivileged {
		return errors.New("gin-service requires master_chroot=n and master_unpriv=n")
	}
	if master.ServiceType != "sock" && master.ServiceType != "inet" && master.ServiceType != "unix" {
		return errors.New("unsupported acl_master service type")
	}
	n, e := closeInheritedSockets(master.SocketCount)
	if e != nil {
		return e
	}
	if n > 0 {
		log.Printf("gin-service closed %d unrelated inherited sockets", n)
	}
	return nil
}
