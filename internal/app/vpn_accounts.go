package app

import (
	"errors"
	"slices"

	"sgate/internal/admin"
	"sgate/internal/cli"
)

func (c *controller) manageVPNAccounts(change *admin.VPNAccountChange) (admin.VPNAccountsSnapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if change != nil && (c.ctx.Err() != nil || c.restartPending) {
		return admin.VPNAccountsSnapshot{}, errors.New("服务正在停止或重启，无法保存 VPN 账号")
	}
	var override *string
	key := admin.VPNAccountFileKey(c.role)
	if slices.Contains(c.overrides, key) {
		fs := cli.CloneFlags(c.original)
		if err := cli.Parse(fs, c.args); err != nil {
			return admin.VPNAccountsSnapshot{}, errors.New("无法读取 VPN 账号文件的命令行配置")
		}
		value := fs.Lookup(key).Value.String()
		override = &value
	}
	view, err := admin.ManageVPNAccountFile(c.path, c.role, c.current.accountsFile, override, change)
	if err != nil || change == nil || c.role != "server" || c.current.vpn == nil || c.worker == nil || c.worker.vpn == nil {
		return view, err
	}
	worker := c.worker.vpn
	worker.mu.Lock()
	defer worker.mu.Unlock()
	if worker.health.State != "starting" && worker.health.State != "listening" {
		return view, nil
	}
	prepared, err := c.current.vpn.WithServerAccounts(view.Path, view.ServerAccounts)
	if err != nil {
		return view, err
	}
	if worker.server == nil {
		worker.serverAccounts = &prepared.Config
	} else {
		worker.server.UpdateAccounts(prepared.Config)
	}
	c.current.vpn = prepared
	view.LiveApplied = true
	return view, nil
}
