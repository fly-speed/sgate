package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"time"
)

// restartService 只操作启动时捕获的 -f 服务文件，不使用 HTTP 请求指定的目标。
func (c *controller) restartService(control, socket string, expectedPID int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	managed := c.managed != nil || (c.service != nil && c.service.Managed())
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
	// 独立于代理上下文，允许 master_ctl 在旧代理退出后完成管理请求。
	commandCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	cmd := exec.CommandContext(commandCtx, control, args...)
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
