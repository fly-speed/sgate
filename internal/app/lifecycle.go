package app

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"

	"sgate/internal/aclservice"
	"sgate/internal/admin"
	"sgate/internal/cli"
	"sgate/internal/tunnel"
	"sgate/internal/vpn"
)

// runtimeHooks 区分框架接入方式，避免调用处传入多个含义不明的 nil。
type runtimeHooks struct {
	managed    *aclservice.Pool
	wrapHTTP   func(net.Listener, http.Handler) http.Handler
	httpServer bool
}

// run 管理客户端及旧 HTTP 调用的生命周期；Gin 服务端入口在 cmd/sgate-server。
func run(
	ctx context.Context,
	role string,
	fs *flag.FlagSet,
	args []string,
	adminPath string,
	hooks runtimeHooks,
) error {
	c, err := newRunController(ctx, role, fs, args, adminPath, hooks)
	if err != nil {
		return err
	}
	if role == "client" {
		if err := c.initializeProxyFile(); err != nil {
			return err
		}
	} else if role == "server" {
		if err := c.initializeAccountsFile(); err != nil {
			return err
		}
	}
	next, err := c.load("")
	if err != nil {
		return err
	}
	view := c.managementView()
	c.current = next
	c.worker, err = next.start(c.ctx, c.stats)
	if err != nil {
		return err
	}
	c.generation = 1
	if vpn.ClientRecoveryPending(c.vpnRecoveryPath()) {
		log.Print(vpn.ErrClientRecoveryPending)
	}
	stopAdmin, err := admin.Start(c.ctx, adminPath, view)
	if err != nil {
		_ = c.worker.stop()
		return err
	}
	defer stopAdmin()
	return c.wait()
}

func newRunController(
	ctx context.Context,
	role string,
	fs *flag.FlagSet,
	args []string,
	adminPath string,
	hooks runtimeHooks,
) (*controller, error) {
	c := &controller{
		ctx:      ctx,
		original: fs,
		args:     append([]string(nil), args...),
		role:     role,
		path:     fs.Lookup("config").Value.String(),
		stats:    tunnel.NewStats(),
		managed:  hooks.managed,
		wrapHTTP: hooks.wrapHTTP,
	}
	c.sharedMode = role == "server" && hooks.httpServer && adminPath != ""
	if f := fs.Lookup("f"); f != nil && f.Value.String() != "" {
		// acl_master keys services by the literal -f string. filepath.Abs/Clean
		// would change bin/../conf into conf and stop/restart a different key.
		c.servicePath = f.Value.String()
	}
	var controlErr error
	c.controls, controlErr = processSettings(c.path)
	if controlErr != nil {
		return nil, controlErr
	}
	if c.path == "" && adminPath != "" {
		return nil, errors.New("Web management requires an application -config file")
	}
	overrides := cli.CloneFlags(fs)
	overrides.String("config", "", "")
	if e := overrides.Parse(args); e != nil {
		return nil, e
	}
	overrides.Visit(func(f *flag.Flag) {
		if hooks.managed != nil || ((hooks.httpServer || role == "client") && aclservice.Enabled()) {
			switch f.Name {
			case "f", "n", "t", "s", "c", "u", "v", "alone":
				return
			}
		}
		if f.Name != "config" {
			c.overrides = append(c.overrides, f.Name)
		}
	})
	return c, nil
}

func (c *controller) managementView() admin.View {
	return admin.View{
		Role:          c.role,
		AppConfigPath: c.path,
		Stats:         c.stats,
		Validate: func(path string) error {
			_, e := c.load(path)
			return e
		},
		Apply:                c.apply,
		ProxyListeners:       c.proxyListeners,
		MappingCandidate:     c.mappingCandidate,
		TestProxy:            c.testProxy,
		TestVPN:              c.testVPN,
		ControlVPN:           c.controlVPN,
		RestartVPN:           c.restartVPN,
		VPNRestartPending:    c.vpnRestartPending,
		ManageTunnelAccounts: c.manageTunnelAccounts,
		ManageVPNAccounts:    c.manageVPNAccounts,
		ClientKeyPath:        c.clientKeyPath,
		ServerVPNFilePaths:   c.serverVPNFilePaths,
		ApplyLive:            c.applyLive,
		LiveFields:           c.liveFields(),
		Runtime:              c.status,
		RestartService:       c.restartService,
	}
}

// 共享入口只创建管理处理器与会话清理任务，不创建第二个监听。
func (c *controller) prepareSharedManagement(path string, next *definition, view admin.View) (func(), error) {
	if !c.sharedMode {
		return func() {}, nil
	}
	if err := validateShared(*next); err != nil {
		return nil, err
	}
	handler, stop, err := admin.Shared(c.ctx, path, view)
	if err != nil {
		return nil, err
	}
	c.shared = &sharedServer{admin: handler}
	next.shared = c.shared
	return stop, nil
}

// 等待框架退出时不能持有 c.mu：尚未完成的管理请求可能需要读取运行状态。
func (c *controller) wait() error {
	<-c.ctx.Done()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.worker != nil {
		if e := c.worker.stop(); e != nil {
			return e
		}
	}
	return nil
}
