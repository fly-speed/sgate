//go:build unix

package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sgate/internal/aclservice"
	"sgate/internal/cli"
)

func TestServiceRestartUsesCapturedPathAndRejectsDuplicates(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "arguments")
	control := filepath.Join(dir, "master_ctl")
	quoted := "'" + strings.ReplaceAll(argsPath, "'", "'\"'\"'") + "'"
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + quoted + "\n"
	if err := os.WriteFile(control, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &controller{
		ctx: ctx, managed: &aclservice.Pool{},
		servicePath: filepath.Join(dir, "server ; literal.cf"),
	}
	var accepted atomic.Int32
	var workers sync.WaitGroup
	for range 16 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if c.restartService(control, "/test/control socket", os.Getpid()) == nil {
				accepted.Add(1)
			}
		}()
	}
	workers.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("accepted %d concurrent restarts", accepted.Load())
	}
	want := "-f\n" + c.servicePath + "\n-a\nrestart\n-t\n30\n-s\n/test/control socket\n"
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if b, _ := os.ReadFile(argsPath); string(b) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("control arguments did not preserve the literal captured service path")
}

func TestServiceRestartZeroExitWithoutReplacementIsFailure(t *testing.T) {
	cmd := exec.Command("/usr/bin/true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := &controller{ctx: context.Background(), restartPending: true}
	c.waitRestart(cmd, 10*time.Millisecond)
	if c.restartPending || c.lastError == "" {
		t.Fatal("zero exit code incorrectly acknowledged an unchanged process")
	}
}

func TestServiceRestartFailureAllowsRetry(t *testing.T) {
	c := &controller{ctx: context.Background(), servicePath: "/test/service.cf"}
	if err := c.restartService("/bin/false", "", os.Getpid()); err == nil {
		t.Fatal("standalone mode allowed service restart")
	}
	c.managed = &aclservice.Pool{}
	if err := c.restartService("/usr/bin/true", "", os.Getpid()+1); err == nil || c.restartPending {
		t.Fatal("stale process request was accepted")
	}
	if err := c.restartService(filepath.Join(t.TempDir(), "missing"), "", os.Getpid()); err == nil || c.restartPending {
		t.Fatal("failed executable launch retained pending state")
	}
	if err := c.restartService("/usr/bin/false", "", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		failed := !c.restartPending && c.lastError != ""
		c.mu.Unlock()
		if failed {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("failed master_ctl command did not release pending state")
}

func TestServiceRestartCapturesFrameworkFileInsteadOfApplicationOverride(t *testing.T) {
	dir := t.TempDir()
	servicePath, appPath := filepath.Join(dir, "framework.cf"), filepath.Join(dir, "proxy.conf")
	if err := os.WriteFile(appPath, []byte("target = 127.0.0.1:9\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fs := appFlags("server")
	fs.String("f", "", "framework service file")
	args := []string{"-f", servicePath, "-config", appPath}
	if err := cli.Parse(fs, args); err != nil {
		t.Fatal(err)
	}
	c, err := newRunController(context.Background(), "server", fs, args, "", runtimeHooks{})
	if err != nil {
		t.Fatal(err)
	}
	if c.servicePath != servicePath || c.path == servicePath {
		t.Fatal("framework restart target was replaced by the application -config path")
	}
}

// Embedded methods are deliberately unused: ownership alone controls the early
// restart authorization check, before any executable is launched.
type restartTCPService struct {
	TCPService
	managed bool
}

func (s restartTCPService) Managed() bool { return s.managed }
func TestTCPFrameworkRestartAuthorization(t *testing.T) {
	c := &controller{ctx: context.Background(), servicePath: "/test/client.cf", tcp: restartTCPService{managed: true}}
	if !c.isManaged() {
		t.Fatal("TCP framework ownership was ignored")
	}
	err := c.restartService("", "", os.Getpid())
	if err == nil || strings.Contains(err.Error(), "仅 acl_master") {
		t.Fatalf("wrong managed authorization: %v", err)
	}
	c.tcp = restartTCPService{managed: false}
	if c.isManaged() {
		t.Fatal("standalone TCP service treated as managed")
	}
	if err = c.restartService("", "", os.Getpid()); err == nil || !strings.Contains(err.Error(), "仅 acl_master") {
		t.Fatalf("standalone restart allowed: %v", err)
	}
}
