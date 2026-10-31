package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

// restartService 只操作启动时捕获的 -f 服务文件，不使用 HTTP 请求指定的目标。
func (c *controller) restartService(control, socket string, expectedPID int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	managed := c.isManaged()
	if !managed || c.servicePath == "" {
		return errors.New("仅 acl_master 托管模式支持整服务重启")
	}
	if expectedPID != os.Getpid() {
		return errors.New("服务进程已改变，请刷新配置后再重启")
	}
	if c.ctx.Err() != nil || c.restartPending {
		return errors.New("服务正在重启或停止，请等待恢复")
	}
	if control == "" {
		return errors.New("未配置 master_ctl 路径")
	}
	args := []string{"-f", c.servicePath, "-a", "restart", "-t", "30000"}
	if socket != "" {
		args = append(args, "-s", socket)
	}
	program := control
	rebind := false
	if c.tcp != nil || c.service != nil {
		settings, err := processSettings(c.servicePath)
		if err != nil {
			return err
		}
		if !sameClientAddresses(settings["master_service"], c.serviceListen) {
			stop, start, err := serviceRestartScripts(c.servicePath, c.role)
			if err != nil {
				return err
			}
			rebind = true
			program = "/bin/sh"
			args = clientRebindArguments(stop, start, control, socket, c.servicePath, os.Getpid())
		}
	}
	// 独立于代理上下文，允许 master_ctl 在旧代理退出后完成管理请求。
	commandCtx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	cmd := exec.CommandContext(commandCtx, program, args...)
	cmd.WaitDelay = time.Second
	// master_ctl 不能随当前代理退出而取消，也不能同步等待，否则自重启会互相等待。
	if err := detachRestart(cmd); err != nil {
		cancel()
		return err
	}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("无法启动 master_ctl: %w", err)
	}
	c.restartPending = true
	failure := "master_ctl 重启未完成，请检查服务日志和管理套接字权限"
	if rebind {
		failure = "停止/启动脚本重启未完成，请检查脚本、服务日志和管理套接字权限"
	}
	go func() {
		defer cancel()
		c.waitRestart(cmd, 30*time.Second, failure)
	}()
	return nil
}

// Installed services keep their scripts beside conf/ in bin/. Source-tree
// services use scripts beside the .cf. Always pass the captured service path.
func clientRestartScripts(servicePath string) (string, string, error) {
	return serviceRestartScripts(servicePath, "client")
}

func serviceRestartScripts(servicePath, role string) (string, string, error) {
	if role != "server" {
		role = "client"
	}
	dir := filepath.Dir(servicePath)
	// acl_master may scan conf/service/ instead of conf/ directly.
	if filepath.Base(dir) == "service" && filepath.Base(filepath.Dir(dir)) == "conf" {
		dir = filepath.Dir(dir)
	}
	if filepath.Base(dir) == "conf" {
		dir = filepath.Join(filepath.Dir(dir), "bin")
	}
	stop, start := filepath.Join(dir, "sgate-"+role+"-stop.sh"), filepath.Join(dir, "sgate-"+role+"-start.sh")
	for _, path := range []string{stop, start} {
		info, err := os.Stat(path)
		if err != nil {
			return "", "", fmt.Errorf("服务重启脚本不存在或不可访问：%s", path)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			return "", "", fmt.Errorf("服务重启脚本不可执行：%s", path)
		}
	}
	return stop, start, nil
}

func clientRebindArguments(stop, start, control, socket, path string, oldPID int) []string {
	// All values are positional parameters; none is interpolated as shell code.
	return []string{"-c", `stop=$1; start=$2; old=$3
MASTER_CTL=$4; MASTER_SOCKET=$5; SGATE_CONFIG=$6; MASTER_CTL_TIMEOUT=30000
export MASTER_CTL MASTER_SOCKET SGATE_CONFIG MASTER_CTL_TIMEOUT
printf 'service listener restart: running stop script; old_pid=%s service=%s\n' "$old" "$SGATE_CONFIG" >&2
"$stop"
status=$?
if [ "$status" -ne 0 ]; then
  printf '%s\n' 'service listener restart: stop script failed; start skipped' >&2
  exit "$status"
fi
printf '%s\n' 'service listener restart: stop script completed; waiting for old process exit' >&2
attempt=0
while kill -0 "$old" 2>/dev/null; do
  attempt=$((attempt+1))
  if [ "$attempt" -gt 300 ]; then
    printf '%s\n' 'service listener restart: old process did not exit; start skipped' >&2
    exit 1
  fi
  sleep 0.1
done
printf '%s\n' 'service listener restart: running start script' >&2
"$start"
status=$?
if [ "$status" -ne 0 ]; then
  printf '%s\n' 'service listener restart: start script failed' >&2
fi
exit "$status"`, "sgate-client-rebind", stop, start, strconv.Itoa(oldPID), control, socket, path}
}

func (c *controller) waitRestart(cmd *exec.Cmd, timeout time.Duration, failure ...string) {
	err := cmd.Wait()
	if err == nil {
		// 某些 master_ctl 在管理请求失败时仍返回 0；旧进程未退出不能视为成功。
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-c.ctx.Done():
			return
		case <-timer.C:
			err = errors.New("master_ctl 已退出，但服务进程未被替换")
		}
	}
	c.mu.Lock()
	c.restartPending = false
	c.lastError = "master_ctl 重启未完成，请检查服务日志和管理套接字权限"
	if len(failure) > 0 {
		c.lastError = failure[0]
	}
	c.mu.Unlock()
	log.Printf("master_ctl restart: %v", err)
}
