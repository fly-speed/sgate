package admin

import (
	"errors"
	"fmt"
	"net/http"
	"slices"

	"sgate/internal/tunnel"
)

var ErrTunnelAccountsConflict = errors.New("客户端账号文件已变化，请刷新列表后重试")

type TunnelAccountInfo struct {
	ReverseMappings []tunnel.ReverseMapping `json:"reverse_mappings,omitempty"`
	Username        string                  `json:"username"`
	Disabled        bool                    `json:"disabled"`
}
type TunnelAccountsSnapshot struct {
	ServiceRestartRequired bool                `json:"service_restart_required,omitempty"`
	ListenersChanged       bool                `json:"listeners_changed,omitempty"`
	SyncedFields           map[string]string   `json:"synced_fields,omitempty"`
	Runtime                any                 `json:"runtime,omitempty"`
	CanCreate              bool                `json:"can_create"`
	SetupMessage           string              `json:"setup_message,omitempty"`
	ConfigChanged          bool                `json:"config_changed,omitempty"`
	ConfigRevision         string              `json:"config_revision,omitempty"`
	RequiresRestart        bool                `json:"requires_restart,omitempty"`
	Pending                bool                `json:"pending"`
	PolicyRevision         string              `json:"-"`
	Enabled                bool                `json:"enabled"`
	Path                   string              `json:"path"`
	Revision               string              `json:"revision"`
	Accounts               []TunnelAccountInfo `json:"accounts"`
}
type TunnelAccountChange struct {
	ReverseMappings *[]tunnel.ReverseMapping `json:"reverse_mappings,omitempty"`
	Revision        string                   `json:"revision"`
	Action          string                   `json:"action"`
	Username        string                   `json:"username"`
	Password        string                   `json:"password"`
	Disabled        *bool                    `json:"disabled,omitempty"`
}

// ManageTunnelAccountsFile is called under the app controller lock, binding
// this transaction to the effective CLI-selected file and running proxy.
func ManageTunnelAccountsFile(path string, change *TunnelAccountChange, apply func([]tunnel.Account) error) (TunnelAccountsSnapshot, error) {
	if path == "" {
		return TunnelAccountsSnapshot{Accounts: []TunnelAccountInfo{}}, nil
	}
	raw, err := regularRead(path, 1<<20)
	if err != nil {
		return TunnelAccountsSnapshot{}, errors.New("无法读取客户端账号文件")
	}
	accounts, err := tunnel.DecodeAccounts(raw)
	if err != nil {
		return TunnelAccountsSnapshot{}, err
	}
	if change != nil {
		if change.Revision != digest(raw) {
			return TunnelAccountsSnapshot{}, ErrTunnelAccountsConflict
		}
		if apply == nil {
			return TunnelAccountsSnapshot{}, errors.New("客户端账号在线更新不可用")
		}
		accounts, err = editTunnelAccounts(accounts, change)
		if err != nil {
			return TunnelAccountsSnapshot{}, err
		}
		next, err := tunnel.EncodeAccounts(accounts)
		if err != nil {
			return TunnelAccountsSnapshot{}, err
		}
		if err := persistTunnelAccounts(path, raw, next, func() error { return apply(accounts) }); err != nil {
			return TunnelAccountsSnapshot{}, err
		}
		raw = next
	}
	return accountSnapshot(path, raw, accounts), nil
}

func accountSnapshot(path string, raw []byte, accounts []tunnel.Account) TunnelAccountsSnapshot {
	canonical, _ := tunnel.EncodeAccounts(accounts)
	snapshot := TunnelAccountsSnapshot{Enabled: true, CanCreate: true, Path: path, Revision: digest(raw), PolicyRevision: digest(canonical), Accounts: make([]TunnelAccountInfo, 0, len(accounts))}
	for _, account := range accounts {
		snapshot.Accounts = append(snapshot.Accounts, TunnelAccountInfo{Username: account.Username, Disabled: account.Disabled, ReverseMappings: slices.Clone(account.ReverseMappings)})
	}
	return snapshot
}

func (h *Handler) tunnelAccounts(w http.ResponseWriter, r *http.Request) {
	if h.view.Role != "server" || h.view.ManageTunnelAccounts == nil {
		problem(w, 404, "客户端账号管理不可用")
		return
	}
	var change *TunnelAccountChange
	if r.Method == http.MethodPut {
		change = &TunnelAccountChange{}
		if !decode(w, r, change, 131072) {
			return
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	// Session validity is rechecked after waiting for another management save.
	if _, err := h.readSession(r); err != nil {
		problem(w, 401, "登录已过期，请重新登录")
		return
	}
	before, _ := regularRead(h.view.AppConfigPath, 1<<20)
	snapshot, err := h.view.ManageTunnelAccounts(change)
	if err != nil {
		status := 400
		if errors.Is(err, ErrTunnelAccountsConflict) {
			status = 409
		}
		problem(w, status, err.Error())
		return
	}
	if snapshot.ConfigChanged {
		baseline := h.startupContent
		if len(baseline) == 0 {
			baseline = before
		}
		updated, e := accountFileConfig(baseline, snapshot.Path)
		if e != nil {
			problem(w, 500, "账号已启用，但无法更新配置状态，请重新加载页面")
			return
		}
		h.startupContent = []byte(updated)
		h.startupRevision = h.restartDigest(h.startupContent)
		if current, e := regularRead(h.view.AppConfigPath, 1<<20); e == nil {
			snapshot.ConfigRevision = digest(current)
			snapshot.RequiresRestart = h.restartDigest(current) != h.startupRevision
		}
	}
	if change != nil && !snapshot.Enabled {
		problem(w, 400, "请先配置 accounts-file 并应用服务端代理")
		return
	}
	if snapshot.ListenersChanged {
		if !snapshot.ServiceRestartRequired && len(h.startupContent) > 0 {
			if updated, err := updateConfigFields(h.startupContent, snapshot.SyncedFields); err == nil {
				h.startupContent = []byte(updated)
				h.startupRevision = h.restartDigest(h.startupContent)
			}
		}
		if current, err := regularRead(h.view.AppConfigPath, 1<<20); err == nil {
			snapshot.ConfigRevision = h.configRevision(current)
		}
	}
	if snapshot.ServiceRestartRequired {
		snapshot.RequiresRestart = true
	}
	if h.view.Runtime != nil {
		snapshot.Runtime = h.view.Runtime()
	}
	reply(w, 200, snapshot)
}

func editTunnelAccounts(accounts []tunnel.Account, change *TunnelAccountChange) ([]tunnel.Account, error) {
	index := slices.IndexFunc(accounts, func(account tunnel.Account) bool { return account.Username == change.Username })
	switch change.Action {
	case "create":
		if index >= 0 {
			return nil, errors.New("客户端账号已存在")
		}
		account := tunnel.Account{Username: change.Username, Password: change.Password}
		if change.Disabled != nil {
			account.Disabled = *change.Disabled
		}
		if change.ReverseMappings != nil {
			account.ReverseMappings = slices.Clone(*change.ReverseMappings)
		}
		accounts = append(accounts, account)
	case "update":
		if index < 0 {
			return nil, errors.New("客户端账号不存在")
		}
		if change.ReverseMappings != nil {
			accounts[index].ReverseMappings = slices.Clone(*change.ReverseMappings)
		}
		if change.Password != "" {
			accounts[index].Password = change.Password
		}
		if change.Disabled != nil {
			accounts[index].Disabled = *change.Disabled
		}
	case "delete":
		if index < 0 {
			return nil, errors.New("客户端账号不存在")
		}
		accounts = append(accounts[:index], accounts[index+1:]...)
	case "reload":
	default:
		return nil, errors.New("客户端账号操作无效")
	}
	return accounts, nil
}

func persistTunnelAccounts(path string, original, next []byte, apply func() error) error {
	latest, err := regularRead(path, 1<<20)
	if err != nil || digest(latest) != digest(original) {
		return ErrTunnelAccountsConflict
	}
	if err := atomicFile(path, next); err != nil {
		// A failed directory fsync can occur after rename; restore only if needed.
		current, readErr := regularRead(path, 1<<20)
		if readErr != nil || digest(current) != digest(original) {
			if restore := atomicFile(path, original); restore != nil {
				return errors.New("账号文件写入失败，且无法恢复原文件")
			}
		}
		return errors.New("无法保存客户端账号文件")
	}
	if err := apply(); err != nil {
		if restore := atomicFile(path, original); restore != nil {
			return errors.New("账号更新失败，且无法恢复账号文件")
		}
		return fmt.Errorf("账号未生效，已恢复原账号文件：%w", err)
	}
	return nil
}
