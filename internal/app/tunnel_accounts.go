package app

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"slices"
	"strings"

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
	var serviceRestart, listenersChanged bool
	var syncedListeners string
	if change != nil {
		if c.ctx.Err() != nil || c.restartPending || c.worker == nil || c.worker.server == nil {
			return admin.TunnelAccountsSnapshot{}, errors.New("代理未运行，不能更新客户端账号")
		}
		if change.ReverseMappings != nil {
			candidate := []tunnel.Account{{ReverseMappings: *change.ReverseMappings}}
			if err := admin.ValidateReverseTunnelPorts(candidate, c.current.listen, c.worker.listener.Addr().String()); err != nil {
				return admin.TunnelAccountsSnapshot{}, err
			}
		}
		previous := []admin.TunnelAccountInfo{}
		if c.current.accountsFile != "" {
			saved, err := admin.ManageTunnelAccountsFile(c.current.accountsFile, nil, nil)
			if err != nil {
				return admin.TunnelAccountsSnapshot{}, err
			}
			previous = saved.Accounts
		}
		// Include the effective policy when a prior saved port change is pending.
		for _, account := range c.current.server.Accounts {
			previous = append(previous, admin.TunnelAccountInfo{Disabled: account.Disabled, ReverseMappings: account.ReverseMappings})
		}
		apply = func(accounts []tunnel.Account) error {
			if err := c.worker.server.ValidateAccounts(accounts); err != nil {
				return err
			}
			if err := admin.ValidateReverseTunnelPorts(accounts, c.current.listen, c.worker.listener.Addr().String()); err != nil {
				return err
			}
			path := c.servicePath
			if path == "" {
				path = c.path
			}
			var err error
			syncedListeners, listenersChanged, err = admin.SyncReverseListeners(path, previous, accounts, func(addresses string, changed bool) error {
				serviceRestart = c.isManaged() && addresses != "" && !sameClientAddresses(addresses, c.serviceListen)
				if serviceRestart {
					// The restart loads the saved configuration, whose tunnel
					// certificate may differ from the currently active one.
					if _, err := c.load(""); err != nil {
						return err
					}
					if c.servicePath == "" || c.service == nil {
						return errors.New("缺少 acl_master 服务文件，无法自动重启")
					}
					if _, _, err := serviceRestartScripts(c.servicePath, "server"); err != nil {
						return err
					}
					if err := checkManagedMappingPorts(strings.ReplaceAll(strings.ReplaceAll(addresses, "|", ":"), " ", ""), c.service.Addresses()); err != nil {
						return err
					}
					return nil
				}
				if err := c.worker.server.UpdateAccounts(accounts); err != nil {
					return err
				}
				current := *c.current.server
				current.Accounts = slices.Clone(accounts)
				c.current.server = &current
				if addresses != "" {
					if c.controls == nil {
						c.controls = map[string]string{}
					}
					c.controls["master_service"] = addresses
				}
				return nil
			})
			if err == nil {
				c.pendingListeners = ""
				if serviceRestart {
					c.pendingListeners = strings.ReplaceAll(strings.ReplaceAll(syncedListeners, "|", ":"), " ", "")
				}
			}
			return err
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
		snapshot.ServiceRestartRequired = c.pendingListeners != "" && c.isManaged()
		snapshot.ListenersChanged = listenersChanged
		if listenersChanged {
			snapshot.SyncedFields = map[string]string{"master_service": syncedListeners}
		}
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

func reverseAddresses(accounts []tunnel.Account) []string {
	var addresses []string
	for _, account := range accounts {
		if !account.Disabled {
			for _, mapping := range account.ReverseMappings {
				addresses = append(addresses, mapping.Listen)
			}
		}
	}
	return addresses
}
