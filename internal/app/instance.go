package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"sgate/internal/aclservice"
	"sgate/internal/tunnel"
	"sgate/internal/vpn"
)

// definition 是一次配置加载的结果；instance 拥有这次运行需要释放的资源。
type definition struct {
	vpn                            *vpn.Prepared
	vpnOnly                        bool
	accountsFile                   string
	commonUsername, commonPassword string
	listen                         string
	quicListen                     string
	client                         *tunnel.ClientConfig
	proxies                        []clientProxy
	server                         *tunnel.ServerConfig
	tls                            *tls.Config
	managed                        *aclservice.Pool
	wrapHTTP                       func(net.Listener, http.Handler) http.Handler
	service                        HTTPService
	tcp                            TCPService
	shared                         *sharedServer
}
type instance struct {
	vpnIdentity     string
	vpnRecoveryPath string
	openTUN         func(string) (vpnDevice, error)
	vpn             *vpnWorker
	quic            *quicWorker
	quicHTTP        *http.Server
	quicListener    net.Listener
	quicDone        chan struct{}
	proxies         *clientProxyGroup
	live            *tunnel.LiveSettings
	cancel          context.CancelFunc
	listener        net.Listener
	server          *tunnel.Server
	http            *http.Server
	done            chan struct{}
	err             error
	service         HTTPWorker
}

func (d definition) start(ctx context.Context, stats *tunnel.Stats) (*instance, error) {
	i, err := d.startProxy(ctx, stats)
	if err == nil && d.vpn != nil && d.vpn.Role != "client" {
		i.startVPN(ctx, d.vpn)
	}
	return i, err
}
func (d definition) startProxy(ctx context.Context, stats *tunnel.Stats) (*instance, error) {
	options := d.resourceOptions()
	if err := stats.MemoryBudget().Configure(uint64(options.MemoryBudgetBytes)); err != nil {
		return nil, err
	}
	live, err := tunnel.NewLiveSettings(d.liveConfig())
	if err != nil {
		return nil, err
	}
	d = d.withLiveSettings(live)
	if len(d.proxies) > 1 && d.tcp == nil {
		return nil, errors.New("multi-proxy requires the go-service TCP lifecycle")
	}
	if d.tcp != nil && d.tcp.Managed() && !d.vpnOnly {
		if err := validateProxyListeners(d.clientRoutes(d.listen), d.tcp.Addresses()); err != nil {
			return nil, err
		}
	}
	if d.service != nil {
		i, err := d.startService(stats)
		if i != nil {
			i.live = live
		}
		return i, err
	}
	var l net.Listener
	var e error
	if d.tcp != nil {
		l, e = d.tcp.Configure(d.listen)
	} else if d.managed != nil {
		l, e = d.managed.Lease()
	} else {
		l, e = net.Listen("tcp", d.listen)
	}
	if e != nil {
		return nil, e
	}
	runCtx, cancel := context.WithCancel(ctx)
	i := &instance{live: live, cancel: cancel, listener: l, done: make(chan struct{})}
	if d.vpnOnly && d.client != nil {
		go func() {
			defer close(i.done)
			for {
				conn, err := l.Accept()
				if err != nil {
					return
				}
				conn.Close()
			}
		}()
	} else if len(d.proxies) > 0 || d.client != nil {
		group, err := newClientProxyGroup(runCtx, l, d.clientRoutes(l.Addr().String()))
		if err != nil {
			l.Close()
			cancel()
			return nil, err
		}
		i.proxies = group
		go func() { i.err = group.run(); close(i.done) }()
	} else {
		s, e := startTunnelServer(*d.server)
		if e != nil {
			l.Close()
			cancel()
			return nil, e
		}
		i.server = s
		transport := stats.Listener(l)
		if d.tls != nil {
			transport = tls.NewListener(transport, d.tls.Clone())
		}
		var handler http.Handler = s
		if d.vpnOnly {
			handler = http.NotFoundHandler()
		}
		if d.wrapHTTP != nil {
			handler = d.wrapHTTP(transport, handler)
		}
		if e := i.startQUIC(d, s, d.tls, stats); e != nil {
			s.Close()
			l.Close()
			cancel()
			return nil, e
		}
		i.http = &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       60 * time.Second,
			MaxHeaderBytes:    16 * 1024,
		}
		go func() {
			e := i.http.Serve(transport)
			if e == http.ErrServerClosed || errors.Is(e, net.ErrClosed) {
				e = nil
			}
			i.err = e
			close(i.done)
		}()
	}
	return i, nil
}
func (i *instance) stop() error {
	i.vpn.stop()
	i.stopQUIC()
	if i.service != nil {
		e := i.service.Close()
		<-i.done
		return e
	}
	i.cancel()
	_ = i.listener.Close()
	if i.server != nil {
		i.server.Close()
	}
	if i.http != nil {
		_ = i.http.Close()
	}
	select {
	case <-i.done:
		return i.err
	case <-time.After(7 * time.Second):
		return errStopTimeout
	}
}

// startService 把 HTTP/TLS 和监听的生命周期交给 gin-service。
func (d definition) startService(stats *tunnel.Stats) (*instance, error) {
	cfg := *d.server
	if d.service.Managed() {
		provider, ok := d.service.(interface {
			ReverseListener(string, string) (net.Listener, error)
		})
		if !ok && len(reverseAddresses(cfg.Accounts)) > 0 {
			return nil, errors.New("HTTP framework cannot lease reverse listeners")
		}
		if ok {
			cfg.ReverseListen = provider.ReverseListener
		}
	}
	s, e := startTunnelServer(cfg)
	if e != nil {
		return nil, e
	}
	var handler http.Handler = s
	if d.vpnOnly {
		handler = http.NotFoundHandler()
	}
	tlsConfig, onStop := d.tls, s.Close
	if d.shared != nil {
		d.shared.vpnOnly.Store(d.vpnOnly)
		d.shared.proxy.Store(s)
		handler, tlsConfig, onStop = d.shared, d.shared.TLSConfig(d.tls), d.shared.Close
	}
	i := &instance{cancel: func() {}, server: s, done: make(chan struct{})}
	if e := i.startQUIC(d, handler, tlsConfig, stats); e != nil {
		s.Close()
		return nil, e
	}
	stopHTTP := onStop
	onStop = func() { i.stopQUIC(); stopHTTP() }
	w, e := d.service.Configure(HTTPConfig{
		AllowUnusedListeners: d.vpnOnly,
		Addresses:            d.listen,
		TLS:                  tlsConfig,
		Handler:              handler,
		WrapListener:         stats.Listener,
		OnStop:               onStop,
	})
	if e != nil {
		i.stopQUIC()
		s.Close()
		return nil, fmt.Errorf("configure HTTP listeners (listen=%q accounts-file=%q active-reverse-listeners=%v vpn-only=%t): %w",
			d.listen, d.accountsFile, reverseAddresses(cfg.Accounts), d.vpnOnly, e)
	}
	i.listener, i.service = w.PrimaryListener(), w
	go func() {
		i.err = w.Wait()
		close(i.done)
	}()
	return i, nil
}

func startTunnelServer(cfg tunnel.ServerConfig) (*tunnel.Server, error) {
	s, err := tunnel.NewServer(cfg)
	if err != nil {
		return nil, err
	}
	if err = s.StartReverse(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (d definition) resourceOptions() tunnel.Options {
	if d.server != nil {
		return d.server.Options
	}
	if d.client != nil {
		return d.client.Options
	}
	return d.proxies[0].config.Options
}
