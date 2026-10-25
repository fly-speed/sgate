package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
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
	args := []string{"-f", c.servicePath, "-a", "restart", "-t", "30"}
	if socket != "" {
		args = append(args, "-s", socket)
	}
	program := control
	if c.tcp != nil {
		settings, err := processSettings(c.servicePath)
		if err != nil {
			return err
		}
		if settings["master_service"] != c.serviceListen {
			// acl_master restart 保留原监听描述符。端口集合改变时必须先
			// stop 再 start，让框架重新打开端口；子进程独立存活至操作结束。
			// 所有路径通过位置参数传入，不能拼入 shell 程序文本。
			program = "/bin/sh"
			args = rebindArguments(control, socket, c.servicePath, os.Getpid())
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
	go func() {
		defer cancel()
		c.waitRestart(cmd, 30*time.Second)
	}()
	return nil
}

func rebindArguments(control, socket, path string, oldPID int) []string {
	// stop 只确认请求已受理；旧进程退出前仍持有监听 socket。
	// 等待其被框架回收后才能 start，否则会出现部分端口绑定失败。
	args := []string{"-c", `control=$1; old=$2; shift 2
"$control" "$@" -a stop || exit $?
attempt=0
while kill -0 "$old" 2>/dev/null; do
  attempt=$((attempt+1))
  [ "$attempt" -le 300 ] || exit 1
  sleep 0.1
done
exec "$control" "$@" -a start`,
		"sgate-rebind", control, strconv.Itoa(oldPID), "-f", path, "-t", "30"}
	if socket != "" {
		args = append(args, "-s", socket)
	}
	return args
}

func (c *controller) waitRestart(cmd *exec.Cmd, timeout time.Duration) {
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
	c.mu.Unlock()
	log.Printf("master_ctl restart: %v", err)
}
