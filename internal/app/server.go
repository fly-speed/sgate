package app

import (
	"context"
	"flag"
	"net/http"
)

// Server 管理代理、配置和持久登录，不创建 Gin 路由，也不运行框架事件循环。
type Server struct {
	controller *controller
	initial    definition
	adminPath  string
	stopAdmin  func()
}

func NewServer(ctx context.Context, fs *flag.FlagSet, args []string, adminPath string) (*Server, error) {
	c, err := newRunController(ctx, "server", fs, args, adminPath, runtimeHooks{httpServer: true})
	if err != nil {
		return nil, err
	}
	if err := c.initializeAccountsFile(); err != nil {
		return nil, err
	}
	next, err := c.load("")
	if err != nil {
		return nil, err
	}
	return &Server{controller: c, initial: next, adminPath: adminPath, stopAdmin: func() {}}, nil
}

func (s *Server) Listen() string { return s.initial.listen }
func (s *Server) ProxyPath() string {
	if s.initial.server.Path == "" {
		return "/tunnel"
	}
	return s.initial.server.Path
}

// Configure 接上已初始化的 gin-service；监听、TLS 和退出仍由框架负责。
func (s *Server) Configure(service HTTPService) error {
	c := s.controller
	c.service, c.ctx = service, service.Context()
	if service.Managed() {
		settings, err := processSettings(c.servicePath)
		if err != nil {
			return err
		}
		c.serviceListen = settings["master_service"]
	}
	next := s.initial
	next.service = service
	stopAdmin, err := c.prepareSharedManagement(s.adminPath, &next, c.managementView())
	if err != nil {
		return err
	}
	s.stopAdmin = stopAdmin
	c.current = next
	c.worker, err = next.start(c.ctx, c.stats)
	if err != nil {
		return err
	}
	c.generation = 1
	return nil
}

func (s *Server) ManagementHandler() http.Handler {
	if s.controller.shared == nil {
		return nil
	}
	return s.controller.shared.admin
}

// 调用方先等待 gin-service Run 返回，再释放管理后台和代理实例。
func (s *Server) Close() {
	if s.controller.worker != nil {
		_ = s.controller.worker.stop()
	}
	s.stopAdmin()
}

func (s *Server) Err() error { return s.controller.service.Err() }
