//go:build unix

package app

import (
	"context"
	"errors"
	"fmt"
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
	want := "-f\n" + c.servicePath + "\n-a\nrestart\n-t\n30000\n-s\n/test/control socket\n"
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

func TestTCPPortChangeRecreatesFrameworkListeners(t *testing.T) {
	for _, failStop := range []bool{false, true} {
		t.Run(fmt.Sprint("fail-stop-", failStop), func(t *testing.T) {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "conf"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(dir, "bin"), 0700); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"sgate-client-stop.sh", "sgate-client-start.sh"} {
				data, err := os.ReadFile(filepath.Join("../..", name))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "bin", name), data, 0700); err != nil {
					t.Fatal(err)
				}
			}
			service := dir + "/bin/../conf/client ; ' literal.cf"
			if err := os.WriteFile(service, []byte("service client {\nmaster_service = 127.0.0.1|9002\n}\n"), 0600); err != nil {
				t.Fatal(err)
			}
			control := filepath.Join(dir, "control ; ' literal")
			// 每次记录精确参数；stop 失败时不得继续 start。
			script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> \"$0.args\"\n"
			if failStop {
				script += "exit 7\n"
			}
			if err := os.WriteFile(control, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			old := exec.Command("/bin/sleep", "0.2")
			if err := old.Start(); err != nil {
				t.Fatal(err)
			}
			reaped := make(chan struct{})
			go func() { _ = old.Wait(); close(reaped) }()
			stop, start, err := clientRestartScripts(service)
			if err != nil {
				t.Fatal(err)
			}
			command := exec.Command("/bin/sh", clientRebindArguments(stop, start, control, "/test/socket ' ; literal", service, old.Process.Pid)...)
			if err := command.Run(); (err != nil) != failStop {
				t.Fatalf("stop result: %v", err)
			}
			if !failStop {
				select {
				case <-reaped:
				default:
					t.Fatal("start ran before old process was reaped")
				}
			}
			<-reaped
			suffix := "\n-t\n30000\n-s\n/test/socket ' ; literal\n"
			want := "-f\n" + service + "\n-a\nstop" + suffix
			if !failStop {
				want += "-f\n" + service + "\n-a\nstart" + suffix
			}
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				b, _ := os.ReadFile(control + ".args")
				if string(b) == want {
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			t.Fatal("listener replacement did not preserve arguments or stop/start ordering")
		})
	}
}

func TestClientRestartScriptsPreflight(t *testing.T) {
	for _, layout := range []string{"source", "conf", "conf/service"} {
		t.Run(layout, func(t *testing.T) {
			root := t.TempDir()
			dir, bin := root, root
			if layout != "source" {
				dir = filepath.Join(root, layout)
				bin = filepath.Join(root, "bin")
			}
			for _, path := range []string{dir, bin} {
				if err := os.MkdirAll(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			service := filepath.Join(dir, "sgate-client.cf")
			if _, _, err := clientRestartScripts(service); err == nil {
				t.Fatal("missing scripts accepted")
			}
			stop, start := filepath.Join(bin, "sgate-client-stop.sh"), filepath.Join(bin, "sgate-client-start.sh")
			if err := os.WriteFile(stop, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
				t.Fatal(err)
			}
			if _, _, err := clientRestartScripts(service); err == nil {
				t.Fatal("missing start accepted")
			}
			if err := os.WriteFile(start, []byte("#!/bin/sh\nexit 0\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := clientRestartScripts(service); err == nil {
				t.Fatal("nonexecutable start accepted")
			}
			if err := os.Chmod(start, 0700); err != nil {
				t.Fatal(err)
			}
			a, b, err := clientRestartScripts(service)
			if err != nil || a != stop || b != start {
				t.Fatalf("wrong scripts %q %q %v", a, b, err)
			}
		})
	}
}

func TestClientPortRestartRejectsMissingScriptsBeforeStop(t *testing.T) {
	dir := t.TempDir()
	service := filepath.Join(dir, "client.cf")
	if err := os.WriteFile(service, []byte("service client {\nmaster_service = 127.0.0.1|9002\n}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	control := filepath.Join(dir, "control")
	if err := os.WriteFile(control, []byte("#!/bin/sh\ntouch \"$0.called\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	c := &controller{ctx: context.Background(), servicePath: service,
		serviceListen: "127.0.0.1|9001", tcp: restartTCPService{managed: true}}
	if err := c.restartService(control, "", os.Getpid()); err == nil || !strings.Contains(err.Error(), "脚本") {
		t.Fatalf("missing scripts did not reject restart: %v", err)
	}
	if c.restartPending {
		t.Fatal("failed preflight left restart pending")
	}
	if _, err := os.Stat(control + ".called"); !os.IsNotExist(err) {
		t.Fatal("preflight ran control before both scripts were checked")
	}
}

func TestClientStartScriptFailureIsReported(t *testing.T) {
	dir := t.TempDir()
	stop, start := filepath.Join(dir, "stop"), filepath.Join(dir, "start")
	for path, content := range map[string]string{stop: "#!/bin/sh\nexit 0\n", start: "#!/bin/sh\nexit 9\n"} {
		if err := os.WriteFile(path, []byte(content), 0700); err != nil {
			t.Fatal(err)
		}
	}
	old := exec.Command("/bin/sleep", "0.01")
	if err := old.Run(); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("/bin/sh", clientRebindArguments(stop, start, "unused", "", "unused", old.Process.Pid)...)
	output, err := command.CombinedOutput()
	var status *exec.ExitError
	if !errors.As(err, &status) || status.ExitCode() != 9 || !strings.Contains(string(output), "start script failed") {
		t.Fatalf("start failure hidden: %v, %s", err, output)
	}
}

func TestServiceRestartPreservesLiteralFrameworkIdentity(t *testing.T) {
	dir := t.TempDir()
	appPath := filepath.Join(dir, "proxy.conf")
	if err := os.WriteFile(appPath, []byte("target = 127.0.0.1:9\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, service := range []string{dir + "/bin/../conf/client.cf", "./bin/../conf/client.cf"} {
		fs := appFlags("server")
		fs.String("f", "", "framework service file")
		args := []string{"-f", service, "-config", appPath}
		if err := cli.Parse(fs, args); err != nil {
			t.Fatal(err)
		}
		c, err := newRunController(context.Background(), "server", fs, args, "", runtimeHooks{})
		if err != nil {
			t.Fatal(err)
		}
		want := service
		if c.servicePath != want {
			t.Fatalf("framework identity normalized: %q != %q", c.servicePath, want)
		}
	}
}
