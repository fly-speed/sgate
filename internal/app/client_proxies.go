package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
