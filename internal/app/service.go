package app

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
)

// HTTPService 只暴露代理所需的 Worker 配置和运行状态，避免共用包引入 Gin。
// Init、路由注册、Run、Close 均由服务端入口显式调用。
type HTTPService interface {
	Configure(HTTPConfig) (HTTPWorker, error)
	Context() context.Context
	Err() error
	Addresses() []string
	Managed() bool
}
type HTTPWorker interface {
	PrimaryListener() net.Listener
	Wait() error
	Close() error
}
type HTTPConfig struct {
	AllowUnusedListeners bool
	Addresses            string
	TLS                  *tls.Config
	Handler              http.Handler
	WrapListener         func(net.Listener) net.Listener
	OnStop               func()
}
