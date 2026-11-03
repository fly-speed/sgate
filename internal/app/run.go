// Package app controls proxy restarts independently of the management listener.
package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"maps"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"sgate/internal/aclservice"
	"sgate/internal/admin"
	"sgate/internal/cli"
	"sgate/internal/serviceconfig"
	"sgate/internal/tunnel"
)

var errStopTimeout = errors.New("proxy shutdown timed out")

type controller struct {
	mu               sync.Mutex
	ctx              context.Context
	original         *flag.FlagSet
	args             []string
	role, path       string
	stats            *tunnel.Stats
	current          definition
	worker           *instance
	generation       int
	lastError        string
	overrides        []string
	managed          *aclservice.Pool
	wrapHTTP         func(net.Listener, http.Handler) http.Handler
	controls         map[string]string
	service          HTTPService
	tcp              TCPService
	tcpClosed        atomic.Uint64
	shared           *sharedServer
	sharedMode       bool
	servicePath      string
	serviceListen    string // 启动时框架持有的 master_service，不能随在线保存改变。
	pendingListeners string
	restartPending   bool
}

func Run(ctx context.Context, role string, fs *flag.FlagSet, args []string, adminPath string) error {
	return run(ctx, role, fs, args, adminPath, runtimeHooks{})
}

// RunACL inherits the official template's sockets once for the whole process.
func RunACL(
	ctx context.Context,
	role string,
	fs *flag.FlagSet,
	args []string,
	adminPath string,
	wrapHTTP func(net.Listener, http.Handler) http.Handler,
) error {
	runtime, e := aclservice.Open(ctx)
	if e != nil {
		return e
	}
	defer runtime.Close()
	return run(
		runtime.Context,
		role,
		fs,
		args,
		adminPath,
		runtimeHooks{managed: runtime.Pool, wrapHTTP: wrapHTTP},
	)
}

func (c *controller) status() any {
	c.mu.Lock()
	defer c.mu.Unlock()
	running := c.worker != nil
	address := ""
	last := c.lastError
	if c.worker != nil {
		address = c.worker.listener.Addr().String()
		select {
		case <-c.worker.done:
			running = false
			if c.worker.err != nil {
				last = c.worker.err.Error()
			}
		default:
		}
	}
	owner := "sgate"
	tcpConnections := 0
	var addresses []string
	if c.service != nil {
		addresses = c.service.Addresses()
		if c.service.Managed() {
			owner = "acl_master"
		}
		if len(addresses) > 0 {
			address = addresses[0]
		}
	}
	if c.tcp != nil {
		addresses = c.tcp.Addresses()
		tcpConnections = c.tcp.ActiveConnections()
		if c.tcp.Managed() {
			owner = "acl_master"
		}
	}
	if c.managed != nil {
		owner = "acl_master"
		addresses = c.managed.Addresses()
	}
	adminRoute := "/"
	if c.sharedMode {
		adminRoute = managementPath
	}
	var pending []string
	if c.pendingListeners != "" {
		pending = strings.Split(c.pendingListeners, ",")
	}
	var websocketListeners []string
	if c.role == "server" {
		websocketListeners = strings.Split(c.current.listen, ",")
		if c.worker != nil {
			websocketListeners = append(websocketListeners, c.worker.listener.Addr().String())
		}
	}
	quic := quicHealth{State: "disabled", Healthy: true}
	if c.worker != nil {
		quic = c.worker.quic.snapshot()
	}
	healthy := running && quic.Healthy
	if !quic.Healthy && quic.Enabled {
		last = quic.LastError
	}
	return map[string]any{
		"healthy": healthy, "quic": quic,
		"websocket_listeners":       websocketListeners,
		"pending_listeners":         pending,
		"noise":                     c.current.noiseStatus(),
		"generation":                c.generation,
		"running":                   running,
		"last_error":                last,
		"listen":                    address,
		"cli_overrides":             c.overrides,
		"listener_owner":            owner,
		"listeners":                 addresses,
		"pid":                       os.Getpid(),
		"http_framework":            c.httpFramework(),
		"tcp_framework":             c.tcpFramework(),
		"tcp_connections":           tcpConnections,
		"tcp_closed_connections":    c.tcpClosed.Load(),
		"proxy_count":               max(1, len(c.current.proxies)),
		"management_shared":         c.sharedMode,
		"management_path":           adminRoute,
		"service_restart_available": owner == "acl_master" && c.servicePath != "",
		"service_restart_pending":   c.restartPending,
	}
}
func (c *controller) apply() error {
	// 与状态查询和其它应用请求串行，确保失败回滚总是针对同一代运行配置。
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ctx.Err() != nil {
		return errors.New("process is stopping")
	}
	if c.restartPending {
		return errors.New("acl_master 正在重启服务，请等待新进程恢复后再应用配置")
	}
	next, e := c.load("")
	if e != nil {
		return e
	}
	if c.isManaged() && next.listen != c.current.listen {
		if c.role == "client" && c.worker != nil && c.worker.proxies != nil && !sameClientAddresses(next.listen, c.current.listen) {
			return c.replaceClientMappings(next)
		}
		return errors.New("监听端口由 acl_master 管理；请修改 .cf 的 master_service 并通过 master_ctl 重启服务")
	}
	controls, e := processSettings(c.path)
	if e != nil {
		return e
	}
	if !maps.Equal(controls, c.controls) {
		return errors.New("框架或管理界面配置已改变；请保存后重启整个进程，代理热重启不能应用这些配置")
	}
	if c.shared != nil {
		return c.replaceShared(next)
	}
	if c.worker != nil {
		if e = c.worker.stop(); errors.Is(e, errStopTimeout) {
			c.lastError = e.Error()
			c.worker = nil
			return e
		}
		c.worker = nil
	}
	worker, e := next.start(c.ctx, c.stats)
	if e != nil {
		rollback, re := c.current.start(c.ctx, c.stats)
		c.worker = rollback
		if re != nil {
			c.lastError = "新配置启动失败，恢复旧配置也失败"
			return fmt.Errorf("%s: %w", c.lastError, re)
		}
		c.lastError = "新配置启动失败，已恢复上次运行配置"
		return fmt.Errorf("%s: %w", c.lastError, e)
	}
	c.current, c.worker = next, worker
	c.generation++
	c.lastError = ""
	return nil
}
func (c *controller) load(candidate string) (definition, error) {
	return c.loadWithMappings(candidate, "", "")
}
func (c *controller) loadWithMappings(candidate, target, staged string) (definition, error) {
	path := candidate
	if path == "" {
		path = c.path
	}
	if path != "" {
		values, merged, e := serviceconfig.Read(path)
		if e != nil {
			return definition{}, e
		}
		if merged {
			if candidate != "" && values["master_args"] != "" {
				if err := c.validateMasterArgs(candidate, values["master_args"], target, staged); err != nil {
					return definition{}, err
				}
			}
			for key := range values {
				if strings.HasPrefix(key, "admin_") {
					loadAdmin := admin.LoadConfig
					if c.role == "server" {
						loadAdmin = admin.LoadSharedConfig
					}
					if _, e := loadAdmin(path); e != nil {
						return definition{}, e
					}
					break
				}
			}
		}
	}
	if candidate != "" {
		check := cli.CloneFlags(c.original)
		// Validate types even for a disk value hidden by a CLI override.
		if e := cli.Parse(check, []string{"-config", candidate}); e != nil {
			return definition{}, e
		}
	}
	fs := cli.CloneFlags(c.original)
	args := append([]string(nil), c.args...)
	if candidate != "" {
		args = append(args, "-config", candidate)
	}
	if e := cli.Parse(fs, args); e != nil {
		return definition{}, e
	}
	useStagedMappings(fs, target, staged)
	d, e := build(c.role, fs, c.stats)
	d.managed = c.managed
	d.tcp = c.tcp
	d.wrapHTTP = c.wrapHTTP
	d.service = c.service
	d.shared = c.shared
	if e == nil && c.sharedMode {
		e = validateShared(d)
	}
	return d, e
}

func processSettings(path string) (map[string]string, error) {
	if path == "" {
		return nil, nil
	}
	values, merged, e := serviceconfig.Read(path)
	if e != nil || !merged {
		return nil, e
	}
	controls := make(map[string]string)
	for key, value := range values {
		if serviceconfig.FrameworkKey(key) || strings.HasPrefix(key, "admin_") {
			controls[key] = value
		}
	}
	return controls, nil
}
func (c *controller) httpFramework() string {
	if c.service != nil {
		return "gin-service"
	}
	return "net/http"
}

func (c *controller) tcpFramework() string {
	if c.tcp != nil {
		return "go-service"
	}
	return "net"
}

// isManaged keeps apply and whole-service restart on the same ownership rule.
func (c *controller) isManaged() bool {
	return c.managed != nil || (c.service != nil && c.service.Managed()) || (c.tcp != nil && c.tcp.Managed())
}
