package admin

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"sgate/internal/tunnel"
)

// SetupTunnelAccounts initializes a previously unconfigured account file. The
// revision belongs to the .cf until an account JSON has been created.
func SetupTunnelAccounts(configPath, accountsPath, blocked string, change *TunnelAccountChange,
	validate func(string) error, apply func([]tunnel.Account) error) (TunnelAccountsSnapshot, error) {
	original, err := regularRead(configPath, 1<<20)
	if err != nil {
		return TunnelAccountsSnapshot{}, errors.New("无法读取代理配置，不能初始化客户端账号")
	}
	snapshot := TunnelAccountsSnapshot{Path: accountsPath, Revision: digest(original), Accounts: []TunnelAccountInfo{}, CanCreate: blocked == "", SetupMessage: blocked}
	if _, err := os.Lstat(accountsPath); err == nil {
		snapshot.CanCreate = false
		snapshot.SetupMessage = "账号文件已存在，请在代理配置中设置 accounts-file 并应用，现有文件不会被覆盖"
	} else if !os.IsNotExist(err) {
		snapshot.CanCreate = false
		snapshot.SetupMessage = "无法检查账号文件位置，请检查目录权限"
	}
	if change == nil {
		return snapshot, nil
	}
	if !snapshot.CanCreate {
		return TunnelAccountsSnapshot{}, errors.New(snapshot.SetupMessage)
	}
	if change.Action != "create" {
		return TunnelAccountsSnapshot{}, errors.New("请先新增第一个客户端账号")
	}
	if change.Revision != snapshot.Revision {
		return TunnelAccountsSnapshot{}, ErrTunnelAccountsConflict
	}
	account := tunnel.Account{Username: change.Username, Password: change.Password}
	if change.Disabled != nil {
		account.Disabled = *change.Disabled
	}
	if change.ReverseMappings != nil {
		account.ReverseMappings = append([]tunnel.ReverseMapping(nil), (*change.ReverseMappings)...)
	}
	accounts := []tunnel.Account{account}
	data, err := tunnel.EncodeAccounts(accounts)
	if err != nil {
		return TunnelAccountsSnapshot{}, err
	}
	updated, err := accountFileConfig(original, accountsPath)
	if err != nil {
		return TunnelAccountsSnapshot{}, err
	}
	if validate == nil || apply == nil {
		return TunnelAccountsSnapshot{}, errors.New("客户端账号初始化不可用")
	}
	// Link a completed file exclusively: a concurrent setup must never be overwritten.
	if err := exclusiveFile(accountsPath, data); err != nil {
		return TunnelAccountsSnapshot{}, errors.New("无法创建账号文件，请检查目录权限或文件是否已存在")
	}
	committed := false
	defer func() {
		if !committed {
			current, err := regularRead(configPath, 1<<20)
			if err == nil && digest(current) == digest(original) {
				_ = os.Remove(accountsPath)
			}
		}
	}()
	candidate, err := os.CreateTemp(filepath.Dir(configPath), ".sgate-account-config-*")
	if err != nil {
		return TunnelAccountsSnapshot{}, errors.New("无法写入代理配置，请检查目录权限")
	}
	name := candidate.Name()
	defer os.Remove(name)
	if err = candidate.Chmod(0600); err == nil {
		_, err = candidate.WriteString(updated)
	}
	if closeErr := candidate.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return TunnelAccountsSnapshot{}, errors.New("无法写入待校验代理配置")
	}
	if err := validate(name); err != nil {
		return TunnelAccountsSnapshot{}, errors.New("账号初始化配置校验失败，原配置已保留")
	}
	if err := persistTunnelAccounts(configPath, original, []byte(updated), func() error { return apply(accounts) }); err != nil {
		return TunnelAccountsSnapshot{}, err
	}
	committed = true
	result := accountSnapshot(accountsPath, data, accounts)
	result.ConfigChanged = true
	result.ConfigRevision = digest([]byte(updated))
	return result, nil
}

func accountFileConfig(original []byte, path string) (string, error) {
	fields, err := configFields(string(original))
	if err != nil {
		return "", err
	}
	for _, field := range fields {
		if field.Key == "accounts-file" {
			return updateConfigFields(original, map[string]string{"accounts-file": path})
		}
	}
	// Older deployments may not yet have this key. Add it inside the service block.
	lines := strings.Split(string(original), "\n")
	entry := "accounts-file = " + strconv.Quote(path)
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "service ") {
			for j := len(lines) - 1; j > i; j-- {
				if strings.TrimSpace(lines[j]) == "}" {
					lines = append(lines[:j], append([]string{"    # Tunnel client accounts; managed through the Web interface.", "    " + entry}, lines[j:]...)...)
					return strings.Join(lines, "\n"), nil
				}
			}
			return "", errors.New("服务配置结构无效")
		}
	}
	return strings.TrimRight(string(original), "\n") + "\n# Tunnel client accounts; managed through the Web interface.\n" + entry + "\n", nil
}
