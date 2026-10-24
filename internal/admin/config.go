// Package admin serves the optional, independent sgate management interface.
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
	"sgate/internal/tunnel"
)

type Config struct {
	Listen, UsersFile, SessionDir, SetupTokenFile, TLSCert, TLSKey string
	WebDir                                                         string
	SessionTimeout                                                 time.Duration
	MaxSessions                                                    int
}

func LoadConfig(path string) (Config, error) {
	var c Config
	fs := flag.NewFlagSet("management", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&c.Listen, "listen", "127.0.0.1:9081", "management address")
	fs.StringVar(&c.WebDir, "web-dir", "../web/admin", "external management HTML, CSS and JavaScript directory")
	fs.StringVar(&c.UsersFile, "users-file", "admin-users.json", "credential file")
	fs.StringVar(&c.SessionDir, "session-dir", "admin-sessions", "persistent session directory")
	fs.StringVar(&c.SetupTokenFile, "setup-token-file", "admin-setup.token", "initial account token file")
	fs.StringVar(&c.TLSCert, "tls-cert", "", "management TLS certificate")
	fs.StringVar(&c.TLSKey, "tls-key", "", "management TLS key")
	fs.DurationVar(&c.SessionTimeout, "session-timeout", 12*time.Hour, "absolute login lifetime")
	fs.IntVar(&c.MaxSessions, "max-sessions", 128, "maximum persisted login sessions")
	if e := cli.Parse(fs, []string{"-config", path}); e != nil {
		return c, e
	}
	for _, p := range []*string{&c.WebDir, &c.UsersFile, &c.SessionDir, &c.SetupTokenFile, &c.TLSCert, &c.TLSKey} {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(filepath.Dir(path), *p)
		}
	}
	return c, c.validate()
}
func (c Config) validate() error {
	if c.SessionTimeout < time.Second || c.SessionTimeout > 365*24*time.Hour || c.MaxSessions < 1 || c.MaxSessions > 10000 {
		return errors.New("invalid management session timeout or limit")
	}
	if c.UsersFile == "" || c.SessionDir == "" || c.SetupTokenFile == "" {
		return errors.New("management credential, session and setup paths are required")
	}
	if c.WebDir == "" {
		return errors.New("management web-dir is required")
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return errors.New("both management TLS files are required")
	}
	host, _, e := net.SplitHostPort(c.Listen)
	if e != nil {
		return e
	}
	ip := net.ParseIP(host)
	if (ip == nil || !ip.IsLoopback()) && c.TLSCert == "" {
		return errors.New("management listening outside loopback requires TLS")
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
	Role, AppConfigPath string
	Stats               *tunnel.Stats
	Validate            func(path string) error
	Apply               func() error
	Runtime             func() any
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
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024}
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
	go func() { defer background.Done(); h.reap(runCtx) }()
	scheme := "http"
	if c.TLSCert != "" {
		scheme = "https"
	}
	log.Printf("sgate %s management: %s://%s", v.Role, scheme, l.Addr())
	var once sync.Once
	return func() { once.Do(func() { cancel(); _ = srv.Close(); <-done; background.Wait() }) }, nil
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
	if e = f.Chmod(0600); e == nil {
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
	dir, e := os.Open(filepath.Dir(path))
	if e != nil {
		return e
	}
	defer dir.Close()
	return dir.Sync()
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
