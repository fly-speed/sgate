// Package app controls proxy restarts independently of the management listener.
package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"maps"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"sgate/internal/aclservice"
	"sgate/internal/admin"
	"sgate/internal/cli"
	"sgate/internal/serviceconfig"
	"sgate/internal/tunnel"
)

var errStopTimeout = errors.New("proxy shutdown timed out")

type definition struct {
	listen   string
	client   *tunnel.ClientConfig
	server   *tunnel.ServerConfig
	tls      *tls.Config
	managed  *aclservice.Pool
	wrapHTTP func(net.Listener, http.Handler) http.Handler
}
type instance struct {
	cancel   context.CancelFunc
	listener net.Listener
	server   *tunnel.Server
	http     *http.Server
	done     chan struct{}
	err      error
}

func (d definition) start(ctx context.Context, stats *tunnel.Stats) (*instance, error) {
	var l net.Listener
	var e error
	if d.managed != nil {
		l, e = d.managed.Lease()
	} else {
		l, e = net.Listen("tcp", d.listen)
	}
	if e != nil {
		return nil, e
	}
	runCtx, cancel := context.WithCancel(ctx)
	i := &instance{cancel: cancel, listener: l, done: make(chan struct{})}
	if d.client != nil {
		c, e := tunnel.NewClient(*d.client)
		if e != nil {
			l.Close()
			cancel()
			return nil, e
		}
		go func() { i.err = c.Run(runCtx, l); close(i.done) }()
	} else {
		s, e := tunnel.NewServer(*d.server)
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
		if d.wrapHTTP != nil {
			handler = d.wrapHTTP(transport, s)
		}
		i.http = &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024}
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

type controller struct {
	mu         sync.Mutex
	ctx        context.Context
	original   *flag.FlagSet
	args       []string
	role, path string
	stats      *tunnel.Stats
	current    definition
	worker     *instance
	generation int
	lastError  string
	overrides  []string
	managed    *aclservice.Pool
	wrapHTTP   func(net.Listener, http.Handler) http.Handler
	controls   map[string]string
}

func Run(ctx context.Context, role string, fs *flag.FlagSet, args []string, adminPath string) error {
	return run(ctx, role, fs, args, adminPath, nil, nil)
}

// RunACL inherits the official template's sockets once for the whole process.
func RunACL(ctx context.Context, role string, fs *flag.FlagSet, args []string, adminPath string, wrapHTTP func(net.Listener, http.Handler) http.Handler) error {
	runtime, e := aclservice.Open(ctx)
	if e != nil {
		return e
	}
	defer runtime.Close()
	return run(runtime.Context, role, fs, args, adminPath, runtime.Pool, wrapHTTP)
}

func run(ctx context.Context, role string, fs *flag.FlagSet, args []string, adminPath string, managed *aclservice.Pool, wrapHTTP func(net.Listener, http.Handler) http.Handler) error {
	c := &controller{ctx: ctx, original: fs, args: append([]string(nil), args...), role: role, path: fs.Lookup("config").Value.String(), stats: tunnel.NewStats(), managed: managed, wrapHTTP: wrapHTTP}
	var controlErr error
	c.controls, controlErr = processSettings(c.path)
	if controlErr != nil {
		return controlErr
	}
	if c.path == "" && adminPath != "" {
		return errors.New("Web management requires an application -config file")
	}
	overrides := cli.CloneFlags(fs)
	overrides.String("config", "", "")
	if e := overrides.Parse(args); e != nil {
		return e
	}
	overrides.Visit(func(f *flag.Flag) {
		if managed != nil {
			switch f.Name {
			case "f", "n", "t", "s", "c", "u", "v", "alone":
				return
			}
		}
		if f.Name != "config" {
			c.overrides = append(c.overrides, f.Name)
		}
	})
	next, e := c.load("")
	if e != nil {
		return e
	}
	c.current = next
	c.worker, e = next.start(ctx, c.stats)
	if e != nil {
		return e
	}
	c.generation = 1
	stopAdmin, e := admin.Start(ctx, adminPath, admin.View{Role: role, AppConfigPath: c.path, Stats: c.stats, Validate: func(path string) error { _, e := c.load(path); return e }, Apply: c.apply, Runtime: c.status})
	if e != nil {
		_ = c.worker.stop()
		return e
	}
	defer stopAdmin()
	<-ctx.Done()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.worker != nil {
		return c.worker.stop()
	}
	return nil
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
	var addresses []string
	if c.managed != nil {
		owner = "acl_master"
		addresses = c.managed.Addresses()
	}
	return map[string]any{"generation": c.generation, "running": running, "last_error": last, "listen": address, "cli_overrides": c.overrides, "listener_owner": owner, "listeners": addresses, "pid": os.Getpid()}
}
func (c *controller) apply() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ctx.Err() != nil {
		return errors.New("process is stopping")
	}
	next, e := c.load("")
	if e != nil {
		return e
	}
	if c.managed != nil && next.listen != c.current.listen {
		return errors.New("监听端口由 acl_master 管理；请修改 .cf 的 master_service 并通过 master_ctl 重启服务")
	}
	controls, e := processSettings(c.path)
	if e != nil {
		return e
	}
	if !maps.Equal(controls, c.controls) {
		return errors.New("框架或管理界面配置已改变；请保存后重启整个进程，代理热重启不能应用这些配置")
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
			for key := range values {
				if strings.HasPrefix(key, "admin_") {
					if _, e := admin.LoadConfig(path); e != nil {
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
	d, e := build(c.role, fs, c.stats)
	d.managed = c.managed
	d.wrapHTTP = c.wrapHTTP
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
func build(role string, fs *flag.FlagSet, stats *tunnel.Stats) (definition, error) {
	text := func(n string) string { return fs.Lookup(n).Value.String() }
	integer := func(n string) int { v, _ := strconv.Atoi(text(n)); return v }
	duration := func(n string) time.Duration { v, _ := time.ParseDuration(text(n)); return v }
	d := definition{listen: text("listen")}
	_, port, e := net.SplitHostPort(d.listen)
	if e != nil {
		return d, e
	}
	if p, e := net.LookupPort("tcp", port); e != nil || p < 0 || p > 65535 {
		return d, errors.New("invalid listen port")
	}
	secret, e := cli.LoadKey(text("key-file"))
	if e != nil {
		return d, e
	}
	opts := tunnel.Options{Stats: stats, BufferBytes: integer("buffer-bytes"), MaxStreams: integer("max-streams"), Heartbeat: duration("heartbeat"), HeartbeatTimeout: duration("heartbeat-timeout"), NoiseInterval: duration("noise-interval"), NoiseIntervalMin: duration("noise-interval-min"), NoiseIntervalMax: duration("noise-interval-max"), NoiseMinBytes: integer("noise-min-bytes"), NoiseMaxBytes: integer("noise-max-bytes")}
	if role == "client" {
		cfg := &tls.Config{MinVersion: tls.VersionTLS12}
		if path := text("tls-ca"); path != "" {
			pem, e := os.ReadFile(path)
			if e != nil {
				return d, e
			}
			roots, e := x509.SystemCertPool()
			if e != nil {
				roots = x509.NewCertPool()
			}
			if !roots.AppendCertsFromPEM(pem) {
				return d, errors.New("no CA certificates found")
			}
			cfg.RootCAs = roots
		}
		cc := tunnel.ClientConfig{Key: secret, Password: text("password"), URL: text("server"), TLSConfig: cfg, ReconnectMin: duration("reconnect-min"), ReconnectMax: duration("reconnect-max"), OfflineTimeout: duration("offline-timeout"), Options: opts}
		if _, e := tunnel.NewClient(cc); e != nil {
			return d, e
		}
		d.client = &cc
	} else {
		cert, key := text("tls-cert"), text("tls-key")
		if (cert == "") != (key == "") {
			return d, errors.New("both TLS certificate and key are required")
		}
		if cert != "" {
			pair, e := tls.LoadX509KeyPair(cert, key)
			if e != nil {
				return d, e
			}
			d.tls = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
		}
		sc := tunnel.ServerConfig{Key: secret, Password: text("password"), Target: text("target"), Path: text("path"), SessionTTL: duration("session-ttl"), MaxSessions: integer("max-sessions"), Options: opts}
		s, e := tunnel.NewServer(sc)
		if e != nil {
			return d, e
		}
		s.Close()
		d.server = &sc
	}
	return d, nil
}
