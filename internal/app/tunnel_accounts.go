package app

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"slices"

	"sgate/internal/admin"
	"sgate/internal/cli"
	"sgate/internal/tunnel"
)

func (c *controller) manageTunnelAccounts(change *admin.TunnelAccountChange) (admin.TunnelAccountsSnapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.role != "server" {
		return admin.TunnelAccountsSnapshot{}, errors.New("客户端账号管理仅在服务端可用")
	}
	var apply func([]tunnel.Account) error
	if change != nil {
		if c.ctx.Err() != nil || c.restartPending || c.worker == nil || c.worker.server == nil {
			return admin.TunnelAccountsSnapshot{}, errors.New("代理未运行，不能更新客户端账号")
		}
		apply = func(accounts []tunnel.Account) error {
			if err := c.worker.server.UpdateAccounts(accounts); err != nil {
				return err
			}
			current := *c.current.server
			current.Accounts = slices.Clone(accounts)
			c.current.server = &current
			return nil
		}
	}
	var snapshot admin.TunnelAccountsSnapshot
	var err error
	if c.current.accountsFile == "" {
		path, blocked, pathErr := c.initialAccountPath()
		if pathErr != nil {
			return snapshot, pathErr
		}
		snapshot, err = admin.SetupTunnelAccounts(c.path, path, blocked, change, func(candidate string) error { _, err := c.load(candidate); return err }, func(accounts []tunnel.Account) error {
			if err := apply(accounts); err != nil {
				return err
			}
			c.current.accountsFile = path
			return nil
		})
	} else {
		snapshot, err = admin.ManageTunnelAccountsFile(c.current.accountsFile, change, apply)
	}
	if err == nil && snapshot.Enabled {
		canonical, _ := tunnel.EncodeAccounts(c.current.server.Accounts)
		sum := sha256.Sum256(canonical)
		snapshot.Pending = hex.EncodeToString(sum[:]) != snapshot.PolicyRevision
	}
	return snapshot, err
}

func (c *controller) initialAccountPath() (string, string, error) {
	configPath, err := filepath.Abs(c.path)
	if err != nil {
		return "", "", errors.New("无法确定代理配置目录")
	}
	path := filepath.Join(filepath.Dir(configPath), "tunnel-accounts.json")
	if slices.Contains(c.overrides, "accounts-file") {
		return path, "命令行将 accounts-file 设为空，请移除此覆盖并重启后再新增账号", nil
	}
	fs := cli.CloneFlags(c.original)
	if err := cli.Parse(fs, c.args); err != nil {
		return "", "", errors.New("无法读取当前代理配置")
	}
	if configured := (configValues{flags: fs}).accountPath(); configured != "" {
		path = configured
	}
	return path, "", nil
}
