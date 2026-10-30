package app

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"

	"sgate/internal/proxyconfig"
	"sgate/internal/tunnel"
)

// 每个监听端口拥有自己的会话、重放缓存和远程 URL；通用安全及心跳参数共用。
type clientProxy struct {
	Listen   string  `json:"listen"`
	Server   string  `json:"server"`
	Addr     string  `json:"addr,omitempty"`
	Username *string `json:"username,omitempty"`
	Password *string `json:"password,omitempty"`
	config   tunnel.ClientConfig
	port     string
}

func (v configValues) multiClientConfigs(secret []byte, options tunnel.Options) ([]clientProxy, error) {
	raw := v.optionalText("proxies")
	path := proxyconfig.Path(v.optionalText("config"), v.optionalText("proxies-file"))
	if path != "" {
		data, err := proxyconfig.Read(path)
		if err != nil {
			return nil, err
		}
		raw = string(data)
	}
	if strings.TrimSpace(raw) == "" && path == "" {
		return nil, nil
	}
	mappings, err := proxyconfig.Decode([]byte(raw))
	if err != nil {
		return nil, err
	}
	if len(mappings) == 0 && path == "" {
		return nil, errors.New("proxies requires 1 to 64 mappings")
	}
	routes := make([]clientProxy, len(mappings))
	for i, m := range mappings {
		routes[i] = clientProxy{Listen: m.Listen, Server: m.Server, Addr: m.Addr, Username: m.Username, Password: m.Password}
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
		if route.Username != nil {
			route.config.Username = *route.Username
		}
		if route.Password != nil {
			route.config.Password = *route.Password
		}
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
