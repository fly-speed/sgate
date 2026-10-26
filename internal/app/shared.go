package app

import (
	"crypto/tls"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"sgate/internal/tunnel"
)

const managementPath = "/admin/"

// sharedServer survives proxy replacement. In particular, an apply request
// never closes or waits for the HTTP worker that is executing that request.
type sharedServer struct {
	mu     sync.Mutex
	closed bool
	admin  http.Handler
	proxy  atomic.Pointer[tunnel.Server]
	tls    atomic.Pointer[tls.Config]
}

func (s *sharedServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if proxy := s.proxy.Load(); proxy != nil {
		proxy.ServeHTTP(w, r)
	} else {
		http.Error(w, "proxy restarting", http.StatusServiceUnavailable)
	}
}

func (s *sharedServer) Close() {
	s.mu.Lock()
	s.closed = true
	proxy := s.proxy.Swap(nil)
	s.mu.Unlock()
	if proxy != nil {
		proxy.Close()
	}
}

func (s *sharedServer) TLSConfig(config *tls.Config) *tls.Config {
	if config == nil {
		return nil
	}
	s.tls.Store(config)
	result := config.Clone()
	result.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) { return s.tls.Load(), nil }
	return result
}

func validateShared(d definition) error {
	if d.server.Path == "/admin" || strings.HasPrefix(d.server.Path, managementPath) {
		return errors.New("proxy path conflicts with /admin/ management routes")
	}
	return nil
}

func (c *controller) replaceShared(next definition) error {
	// 这里由管理 API 同步调用，不能关闭承载当前请求的 HTTP Worker。
	// 只交换业务代理；原代理关闭后释放其业务连接、缓存和传输定时器。
	if next.listen != c.current.listen || (next.tls == nil) != (c.current.tls == nil) {
		return errors.New("共享管理端口的监听地址或 HTTP/HTTPS 模式改变；请保存配置后重启整个进程")
	}
	if e := validateShared(next); e != nil {
		return e
	}
	live, e := tunnel.NewLiveSettings(next.liveConfig())
	if e != nil {
		return e
	}
	next = next.withLiveSettings(live)
	proxy, e := tunnel.NewServer(*next.server)
	if e != nil {
		return e
	}
	c.shared.mu.Lock()
	if c.shared.closed || c.ctx.Err() != nil {
		c.shared.mu.Unlock()
		proxy.Close()
		return errors.New("process is stopping")
	}
	old := c.shared.proxy.Swap(proxy)
	if next.tls != nil {
		c.shared.tls.Store(next.tls)
	}
	c.shared.mu.Unlock()
	if old != nil {
		old.Close()
	}
	c.worker.server = proxy
	c.worker.live = live
	c.current = next
	c.generation++
	c.lastError = ""
	return nil
}
