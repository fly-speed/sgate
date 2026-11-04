// Package desktop owns only the client process launched by this controller.
package desktop

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"sgate/internal/clientcontrol"
	"sgate/internal/serviceconfig"
)

type Status struct {
	State string
	PID   int
	Error string
}
type Client struct {
	mu                      sync.Mutex
	Executable, Config, Log string
	status                  Status
	input                   *os.File
}

func New(root string) *Client {
	return &Client{Executable: filepath.Join(root, "bin", "sgate-client.exe"), Config: filepath.Join(root, "conf", "sgate-client.cf"), Log: filepath.Join(root, "logs", "sgate-client.log"), status: Status{State: "stopped"}}
}
func (c *Client) Snapshot() Status { c.mu.Lock(); defer c.mu.Unlock(); return c.status }
func (c *Client) Start() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.input != nil {
		return errors.New("客户端正在运行或停止中")
	}
	fail := func(err error) error { c.status = Status{State: "failed", Error: err.Error()}; return err }
	for _, name := range []string{c.Executable, c.Config} {
		if st, e := os.Stat(name); e != nil {
			return fail(e)
		} else if !st.Mode().IsRegular() {
			return fail(fmt.Errorf("not a regular file: %s", name))
		}
	}
	if err := os.MkdirAll(filepath.Dir(c.Log), 0700); err != nil {
		return fail(err)
	}
	log, err := os.OpenFile(c.Log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return fail(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		log.Close()
		return fail(err)
	}
	cmd := exec.Command(c.Executable, "-alone", "-f", c.Config, "-desktop-controlled")
	cmd.Dir = filepath.Dir(c.Config)
	cmd.Stdin = r
	cmd.Stdout = log
	cmd.Stderr = log
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.EqualFold(key, clientcontrol.Environment) {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, clientcontrol.Environment+"=1")
	configureProcess(cmd)
	if err = cmd.Start(); err != nil {
		r.Close()
		w.Close()
		log.Close()
		return fail(err)
	}
	r.Close()
	c.input = w
	c.status = Status{State: "running", PID: cmd.Process.Pid}
	go func() {
		err := cmd.Wait()
		w.Close()
		log.Close()
		c.mu.Lock()
		defer c.mu.Unlock()
		c.input = nil
		c.status = Status{State: "stopped"}
		if err != nil {
			c.status = Status{State: "failed", Error: err.Error() + "；请查看日志"}
		}
	}()
	return nil
}
func (c *Client) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.input == nil {
		return
	}
	c.status.State = "stopping"
	// Closing the parent's pipe handle causes EOF in the client's watcher.
	c.input.Close()
}

func (c *Client) ManagementURL() (string, error) {
	values, _, err := serviceconfig.Read(c.Config)
	if err != nil {
		return "", err
	}
	if values["admin-config"] == "" {
		enabled, _ := strconv.ParseBool(values["admin_enabled"])
		if !enabled {
			return "", errors.New("管理页面未启用，请编辑配置设置 admin_enabled = true，然后重启客户端")
		}
	}
	if separate := values["admin-config"]; separate != "" {
		if !filepath.IsAbs(separate) {
			separate = filepath.Join(filepath.Dir(c.Config), separate)
		}
		values, _, err = serviceconfig.Read(separate)
		if err != nil {
			return "", err
		}
		if v := values["listen"]; v != "" {
			values["admin_listen"] = v
		}
		if v := values["tls-cert"]; v != "" {
			values["admin_tls_cert"] = v
		}
	}
	address := values["admin_listen"]
	if address == "" {
		address = "127.0.0.1:9081"
	}
	host, port, err := net.SplitHostPort(address)
	number, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || number < 1 || number > 65535 {
		return "", errors.New("管理监听地址格式无效，请检查 admin_listen")
	}
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	} else if host == "::" {
		host = "::1"
	}
	scheme := "http"
	if values["admin_tls_cert"] != "" {
		scheme = "https"
	}
	return (&url.URL{Scheme: scheme, Host: net.JoinHostPort(host, port), Path: "/"}).String(), nil
}
