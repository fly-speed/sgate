package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"

	"sgate/internal/tunnel"
)

// 每个监听端口拥有自己的会话、重放缓存和远程 URL；通用安全及心跳参数共用。
type clientProxy struct {
	Listen string `json:"listen"`
	Server string `json:"server"`
	Addr   string `json:"addr,omitempty"`
	config tunnel.ClientConfig
	port   string
}

func (v configValues) multiClientConfigs(secret []byte, options tunnel.Options) ([]clientProxy, error) {
	flag := v.flags.Lookup("proxies")
	if flag == nil || strings.TrimSpace(flag.Value.String()) == "" {
		return nil, nil
	}
	var routes []clientProxy
	decoder := json.NewDecoder(strings.NewReader(flag.Value.String()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&routes); err != nil {
		return nil, errors.New("proxies must be a JSON array of listen/server/addr mappings")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("unexpected data after proxies")
	}
	if len(routes) == 0 || len(routes) > 64 {
		return nil, errors.New("proxies requires 1 to 64 mappings")
	}
	seen := make(map[string]bool)
	tlsConfig, err := clientTLSConfig(v.text("tls-ca"))
	if err != nil {
		return nil, err
	}
	common := v.clientSettings(secret, options)
	common.TLSConfig = tlsConfig
	for index := range routes {
		route := &routes[index]
		address, err := net.ResolveTCPAddr("tcp", route.Listen)
		if err != nil || address.Port <= 0 {
			return nil, fmt.Errorf("proxies[%d]: listen requires a valid nonzero TCP port", index)
		}
		route.port = fmt.Sprint(address.Port)
		// 不同监听 IP 上复用同一端口容易与通配监听混淆，映射要求端口唯一。
		if seen[route.port] {
			return nil, errors.New("proxies listen ports must be unique")
		}
		seen[route.port] = true
		route.Listen = address.String()
		route.config = common
		route.config.URL, route.config.Addr = route.Server, route.Addr
		if _, err := tunnel.NewClient(route.config); err != nil {
			return nil, fmt.Errorf("proxies[%d]: %w", index, err)
		}
	}
	return routes, nil
}

// proxyListener 接收框架按本地端口分派的连接，关闭时只停止此队列。
// 已交付的连接由各 tunnel.Client 释放，尚未交付的连接由组关闭路径排空。
type proxyListener struct {
	incoming chan net.Conn
	done     chan struct{}
	once     sync.Once
	address  net.Addr
}

func (l *proxyListener) Accept() (net.Conn, error) {
	select {
	case <-l.done:
		return nil, net.ErrClosed
	case c := <-l.incoming:
		return c, nil
	}
}
func (l *proxyListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *proxyListener) Addr() net.Addr { return l.address }

func runClientProxies(parent context.Context, listener net.Listener, routes []clientProxy) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	listeners := make(map[string]*proxyListener, len(routes))
	clients := make([]*tunnel.Client, 0, len(routes))
	for _, route := range routes {
		client, err := tunnel.NewClient(route.config)
		if err != nil {
			return err
		}
		clients = append(clients, client)
		size := route.config.Options.MaxStreams
		if size <= 0 {
			size = 128
		}
		address, _ := net.ResolveTCPAddr("tcp", route.Listen)
		listeners[route.port] = &proxyListener{incoming: make(chan net.Conn, size), done: make(chan struct{}), address: address}
	}
	// 任一运行循环失败时停止整个工作实例，避免管理界面误报部分启动成功。
	failures := make(chan error, len(routes)+1)
	var workers sync.WaitGroup
	for index, route := range routes {
		workers.Add(1)
		go func(c *tunnel.Client, l *proxyListener) {
			defer workers.Done()
			failures <- c.Run(ctx, l)
			cancel()
		}(clients[index], listeners[route.port])
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		failures <- dispatchClientProxies(ctx, listener, listeners)
		cancel()
	}()
	<-ctx.Done()
	listener.Close()
	for _, l := range listeners {
		l.Close()
	}
	workers.Wait()
	close(failures)
	for _, l := range listeners {
		for len(l.incoming) > 0 {
			(<-l.incoming).Close()
		}
	}
	for err := range failures {
		if err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, context.Canceled) {
			return err
		}
	}
	return nil
}

func dispatchClientProxies(ctx context.Context, listener net.Listener, routes map[string]*proxyListener) error {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		_, port, err := net.SplitHostPort(conn.LocalAddr().String())
		route := routes[port]
		if err != nil || route == nil {
			log.Printf("TCP connection rejected: listen=%q peer=%q reason=no matching proxy mapping", conn.LocalAddr(), conn.RemoteAddr())
			conn.Close()
			continue
		}
		select {
		case <-ctx.Done():
			conn.Close()
			return nil
		case <-route.done:
			if ctx.Err() == nil {
				log.Printf("TCP connection rejected: listen=%q peer=%q reason=proxy mapping stopped", conn.LocalAddr(), conn.RemoteAddr())
			}
			conn.Close()
		case route.incoming <- conn:
		default:
			log.Printf("TCP connection rejected: listen=%q peer=%q reason=proxy accept queue full", conn.LocalAddr(), conn.RemoteAddr())
			conn.Close() // 有界入站队列，单端口过载不能阻塞其他代理。
		}
	}
}
