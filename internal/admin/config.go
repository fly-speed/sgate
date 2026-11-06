// Package admin serves client management separately and server management on the proxy listener.
package admin

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"sgate/internal/cli"
	"sgate/internal/configfile"
	"sgate/internal/tunnel"
	"sgate/internal/vpn"
)

type Config struct {
	Listen, UsersFile, SessionDir, SetupTokenFile, TLSCert, TLSKey string
	WebDir                                                         string
	MasterCtl, MasterSocket                                        string
	SessionTimeout                                                 time.Duration
	MaxSessions                                                    int
	shared                                                         bool
}

func LoadConfig(path string) (Config, error) {
	return loadConfig(path, false)
}

// LoadSharedConfig has no management listen address; the proxy owns HTTP/TLS.
func LoadSharedConfig(path string) (Config, error) { return loadConfig(path, true) }

func loadConfig(path string, shared bool) (Config, error) {
	c := Config{shared: shared}
	fs := flag.NewFlagSet("management", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if shared {
		// 仅兼容读取旧文件；不保存、不验证，也不用于监听或会话身份。
		fs.Func("listen", "obsolete shared management option (ignored)", func(string) error { return nil })
	} else {
		fs.StringVar(&c.Listen, "listen", "127.0.0.1:9081", "client management address")
	}
	fs.StringVar(&c.WebDir, "web-dir", "../web/admin", "external management HTML, CSS and JavaScript directory")
	fs.StringVar(&c.UsersFile, "users-file", "admin-users.json", "credential file")
	fs.StringVar(&c.SessionDir, "session-dir", "admin-sessions", "persistent session directory")
	fs.StringVar(&c.SetupTokenFile, "setup-token-file", "admin-setup.token", "initial account token file")
	fs.StringVar(&c.TLSCert, "tls-cert", "", "management TLS certificate")
	fs.StringVar(&c.TLSKey, "tls-key", "", "management TLS key")
	fs.DurationVar(&c.SessionTimeout, "session-timeout", 12*time.Hour, "absolute login lifetime")
	fs.IntVar(&c.MaxSessions, "max-sessions", 128, "maximum persisted login sessions")
	fs.StringVar(&c.MasterCtl, "master-ctl", "/opt/soft/acl-master/bin/master_ctl", "acl_master service control executable")
	fs.StringVar(&c.MasterSocket, "master-socket", "", "optional acl_master management socket")
	if e := cli.ParseSection(fs, path, "admin_"); e != nil {
		return c, e
	}
	for _, p := range []*string{&c.WebDir, &c.UsersFile, &c.SessionDir, &c.SetupTokenFile, &c.TLSCert, &c.TLSKey} {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(filepath.Dir(path), *p)
		}
	}
	for _, p := range []*string{&c.MasterCtl, &c.MasterSocket} {
		if *p != "" && !filepath.IsAbs(*p) {
			resolved, err := filepath.Abs(filepath.Join(filepath.Dir(path), *p))
			if err != nil {
				return c, err
			}
			*p = resolved
		}
	}
	return c, c.validate()
}
func (c Config) validate() error {
	if c.SessionTimeout < time.Second || c.SessionTimeout > 365*24*time.Hour || c.MaxSessions < 1 ||
		c.MaxSessions > 10000 {
		return errors.New("invalid management session timeout or limit")
	}
	if c.UsersFile == "" || c.SessionDir == "" || c.SetupTokenFile == "" {
		return errors.New("management credential, session and setup paths are required")
	}
	if c.WebDir == "" {
		return errors.New("management web-dir is required")
	}
	if !c.shared && (c.TLSCert == "") != (c.TLSKey == "") {
		return errors.New("both management TLS files are required")
	}
	if !c.shared {
		host, _, e := net.SplitHostPort(c.Listen)
		if e != nil {
			return e
		}
		ip := net.ParseIP(host)
		if (ip == nil || !ip.IsLoopback()) && c.TLSCert == "" {
			return errors.New("management listening outside loopback requires TLS")
		}
	}
	paths := []string{c.UsersFile, c.SessionDir, c.SetupTokenFile}
	for i, p := range paths {
		for j := 0; j < i; j++ {
			a, _ := filepath.Abs(p)
			b, _ := filepath.Abs(paths[j])
			if a == b {
				return errors.New("management storage paths must be distinct")
			}
		}
	}
	return nil
}

type View struct {
	Role, AppConfigPath  string
	Stats                *tunnel.Stats
	Validate             func(path string) error
	ProxyListeners       func(path string) ([]string, error)
	MappingCandidate     func(path, target, staged string) ([]string, error)
	TestProxy            func(context.Context, ProxyMapping) (tunnel.ProbeResult, error)
	ControlVPN           func(bool) error
	RestartVPN           func() error
	VPNRestartPending    func() (pending, retry bool, err error)
	TestVPN              func(context.Context, map[string]string) (vpn.ProbeResult, error)
	ManageTunnelAccounts func(*TunnelAccountChange) (TunnelAccountsSnapshot, error)
	ManageVPNAccounts    func(*VPNAccountChange) (VPNAccountsSnapshot, error)
	ClientKeyPath        func() (string, error)
	ServerVPNFilePaths   func() (map[string]string, error)
	Apply                func() error
	ApplyLive            func([]string) error
	LiveFields           []string
	Runtime              func() any
	RestartService       func(control, socket string, expectedPID int) error
}

// Shared creates only a handler and session reaper. The proxy's HTTP service
// owns the listener and joins requests before this cleanup function returns.
func Shared(ctx context.Context, path string, v View) (http.Handler, func(), error) {
	c, e := LoadSharedConfig(path)
	if e != nil {
		return nil, nil, e
	}
	h, e := New(c, v)
	if e != nil {
		return nil, nil, e
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.reap(runCtx)
	}()
	var once sync.Once
	return h, func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}, nil
}

// Start binds synchronously so management startup errors cannot be hidden.
func Start(ctx context.Context, path string, v View) (func(), error) {
	if path == "" {
		return func() {}, nil
	}
	c, e := LoadConfig(path)
	if e != nil {
		return nil, e
	}
	var certificate tls.Certificate
	if c.TLSCert != "" {
		certificate, e = tls.LoadX509KeyPair(c.TLSCert, c.TLSKey)
		if e != nil {
			return nil, e
		}
	}
	h, e := New(c, v)
	if e != nil {
		return nil, e
	}
	l, e := net.Listen("tcp", c.Listen)
	if e != nil {
		return nil, e
	}
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 * 1024,
	}
	if c.TLSCert != "" {
		srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		var err error
		if c.TLSCert != "" {
			err = srv.ServeTLS(l, "", "")
		} else {
			err = srv.Serve(l)
		}
		if err != nil && err != http.ErrServerClosed {
			log.Printf("management server: %v", err)
		}
		cancel()
	}()
	var background sync.WaitGroup
	background.Add(2)
	go func() {
		defer background.Done()
		<-runCtx.Done()
		shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = srv.Shutdown(shutdown)
	}()
	go func() {
		defer background.Done()
		h.reap(runCtx)
	}()
	scheme := "http"
	if c.TLSCert != "" {
		scheme = "https"
	}
	log.Printf("sgate %s management: %s://%s", v.Role, scheme, l.Addr())
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			_ = srv.Close()
			<-done
			background.Wait()
		})
	}, nil
}
func atomicFile(path string, data []byte) error {
	if st, e := os.Lstat(path); e == nil && !st.Mode().IsRegular() {
		return errors.New("refusing to replace a non-regular file")
	} else if e != nil && !os.IsNotExist(e) {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".sgate-*")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if e = configfile.ProtectFile(f); e == nil {
		_, e = f.Write(data)
	}
	if e == nil {
		e = f.Sync()
	}
	if ce := f.Close(); e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	if e = os.Rename(name, path); e != nil {
		return e
	}
	return configfile.SyncDirectory(filepath.Dir(path))
}

func regularRead(path string, limit int64) ([]byte, error) {
	st, e := os.Lstat(path)
	if e != nil {
		return nil, e
	}
	if !st.Mode().IsRegular() || st.Size() > limit {
		return nil, fmt.Errorf("invalid or oversized management file")
	}
	return os.ReadFile(path)
}
