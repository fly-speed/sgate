package app

import (
	"context"
	"flag"
	"fmt"
	"net"

	"sgate/internal/admin"
)

// TCPService exposes the go-service lifecycle without coupling tunnel code to
// framework globals. Its workers are disposable application admission queues.
type TCPService interface {
	Configure(string) (net.Listener, error)
	AcceptConn(net.Conn)
	ActiveConnections() int
	Context() context.Context
	Addresses() []string
	Managed() bool
	Err() error
}

// Client starts management separately; go-service owns the business TCP port.
type Client struct {
	controller *controller
	initial    definition
	adminPath  string
	stopAdmin  func()
}

func NewClient(ctx context.Context, fs *flag.FlagSet, args []string, adminPath string) (*Client, error) {
	c, err := newRunController(ctx, "client", fs, args, adminPath, runtimeHooks{})
	if err != nil {
		return nil, err
	}
	if err := c.initializeProxyFile(); err != nil {
		return nil, err
	}
	next, err := c.load("")
	if err != nil {
		return nil, err
	}
	return &Client{controller: c, initial: next, adminPath: adminPath, stopAdmin: func() {}}, nil
}
func (c *Client) Listen() string { return c.initial.listen }
func (c *Client) Configure(service TCPService) error {
	controller := c.controller
	controller.tcp, controller.ctx = service, service.Context()
	if service.Managed() {
		settings, err := processSettings(controller.servicePath)
		if err != nil {
			return err
		}
		controller.serviceListen = settings["master_service"]
	}
	next := c.initial
	next.tcp = service
	controller.current = next
	worker, err := next.start(controller.ctx, controller.stats)
	if err != nil {
		return err
	}
	controller.worker, controller.generation = worker, 1
	stop, err := admin.Start(controller.ctx, c.adminPath, controller.managementView())
	if err != nil {
		worker.stop()
		return err
	}
	c.stopAdmin = stop
	return nil
}
func (c *Client) Accept(conn net.Conn) { c.controller.tcp.AcceptConn(conn) }

// Closed 只更新计数，避免大量连接同时关闭时争用全局日志锁。
func (c *Client) Closed(_ net.Conn) { c.controller.tcpClosed.Add(1) }
func (c *Client) Close() {
	// Stop HTTP first so a final Apply cannot race cleanup of the TCP worker.
	c.stopAdmin()
	c.controller.mu.Lock()
	defer c.controller.mu.Unlock()
	if c.controller.worker != nil {
		_ = c.controller.worker.stop()
	}
}
func (c *Client) Err() error { return c.controller.tcp.Err() }

// 托管模式只使用框架继承的端口，不能悄悄遗漏映射或接入未配置的端口。
func validateProxyListeners(routes []clientProxy, addresses []string) error {
	// acl_master may retain its bootstrap listeners while the mapping list is empty.
	if len(routes) == 0 {
		return nil
	}
	want := make(map[string]bool, len(routes))
	seen := make(map[string]bool, len(routes))
	for _, route := range routes {
		want[route.port] = true
	}
	for _, address := range addresses {
		_, port, err := net.SplitHostPort(address)
		if err != nil || !want[port] {
			return fmt.Errorf(
				"master_service listener %q must match a proxies listen port; "+
					"keep the independent admin_listen port out of master_service",
				address,
			)
		}
		// acl_master may expand a wildcard port into one listener per network
		// interface. All of them belong to the same proxy mapping.
		seen[port] = true
	}
	if len(seen) != len(want) {
		return fmt.Errorf("master_service is missing proxies listen ports")
	}
	return nil
}
