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
	return admin.ManageVPNAccountFile(c.path, c.role, c.current.accountsFile, override, change)
}
