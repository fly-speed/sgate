package admin

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"

	"sgate/internal/tunnel"
)

type VPNAccountChange struct {
	Revision    string `json:"revision"`
	Action      string `json:"action"`
	Username    string `json:"username"`
	NewUsername string `json:"new_username,omitempty"`
	Password    string `json:"password"`
	Disabled    *bool  `json:"disabled,omitempty"`
}

type VPNAccountInfo struct {
	Username string `json:"username"`
	Disabled bool   `json:"disabled"`
}

type VPNAccountsSnapshot struct {
	Path                   string           `json:"path"`
	Revision               string           `json:"revision"`
	Accounts               []VPNAccountInfo `json:"accounts"`
	Blocked                string           `json:"blocked,omitempty"`
	ConfigChanged          bool             `json:"config_changed,omitempty"`
	ConfigRevision         string           `json:"config_revision,omitempty"`
	PreviousConfigRevision string           `json:"previous_config_revision,omitempty"`
	ConfigValue            string           `json:"config_value,omitempty"`
}

var errVPNAccountsConflict = errors.New("VPN 账号或配置已变化，请刷新后重试")

func VPNAccountFileKey(role string) string {
	if role == "server" {
		return "vpn-accounts-file"
	}
	return "vpn-credentials-file"
}

func (h *Handler) vpnAccounts(w http.ResponseWriter, r *http.Request) {
	if h.view.ManageVPNAccounts == nil {
		problem(w, 404, "VPN 账号管理不可用")
		return
	}
	var change *VPNAccountChange
	if r.Method == http.MethodPut {
		change = &VPNAccountChange{}
		if !decode(w, r, change, 16<<10) {
			return
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, err := h.readSession(r); err != nil {
		problem(w, 401, "登录已过期，请重新登录")
		return
	}
	var previousRevision string
	if change != nil {
		if before, err := regularRead(h.view.AppConfigPath, 1<<20); err == nil {
			previousRevision = h.configRevision(before)
		}
	}
	view, err := h.view.ManageVPNAccounts(change)
	if err != nil {
		status := 400
		if errors.Is(err, errVPNAccountsConflict) || errors.Is(err, ErrTunnelAccountsConflict) {
			status = 409
		}
		problem(w, status, err.Error())
		return
	}
	if view.ConfigChanged {
		view.PreviousConfigRevision = previousRevision
		if current, err := regularRead(h.view.AppConfigPath, 1<<20); err == nil {
			view.ConfigRevision = h.configRevision(current)
		}
	}
	reply(w, 200, view)
}

// ManageVPNAccountFile is serialized with other configuration mutations by
// the controller and the management handler.
func ManageVPNAccountFile(configPath, role, runningProxyAccounts string, override *string, change *VPNAccountChange) (VPNAccountsSnapshot, error) {
	config, err := regularRead(configPath, 1<<20)
	if err != nil {
		return VPNAccountsSnapshot{}, errors.New("无法读取 VPN 配置")
	}
	fields, err := configFields(string(config))
	if err != nil {
		return VPNAccountsSnapshot{}, err
	}
	key := VPNAccountFileKey(role)
	var configured, proxyAccounts string
	for _, field := range fields {
		switch field.Key {
		case key:
			configured = field.Value
		case "accounts-file":
			proxyAccounts = field.Value
		}
	}
	view := VPNAccountsSnapshot{Accounts: []VPNAccountInfo{}}
	if override != nil {
		configured = *override
		if configured == "" {
			view.Blocked = "命令行将 VPN 账号文件路径设为空"
		}
	}
	path := configured
	if path == "" {
		path = "vpn-client-credentials.json"
		if role == "server" {
			path = "vpn-server-accounts.json"
		}
	}
	resolve := func(path string) string {
		if filepath.IsAbs(path) {
			return filepath.Clean(path)
		}
		return filepath.Join(filepath.Dir(configPath), path)
	}
	view.Path = resolve(path)
	for _, proxy := range []string{proxyAccounts, runningProxyAccounts} {
		if proxy != "" && sameAccountFile(view.Path, resolve(proxy)) {
			view.Blocked = "VPN 必须使用独立于代理的账号文件"
		}
	}
	if view.Blocked != "" {
		if change != nil {
			return VPNAccountsSnapshot{}, errors.New(view.Blocked)
		}
		return view, nil
	}
	raw, err := regularRead(view.Path, 1<<20)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return VPNAccountsSnapshot{}, errors.New("无法读取 VPN 账号文件")
	}
	accounts := []tunnel.Account{}
	if exists {
		if configured == "" {
			view.Blocked = "默认 VPN 账号文件已存在，请在配置页指定该文件路径"
			if change != nil {
				return VPNAccountsSnapshot{}, errors.New(view.Blocked)
			}
			return view, nil
		}
		accounts, err = tunnel.DecodeAccounts(raw)
		if err != nil {
			return VPNAccountsSnapshot{}, err
		}
		if slices.ContainsFunc(accounts, func(a tunnel.Account) bool { return len(a.ReverseMappings) != 0 }) ||
			(role == "client" && (len(accounts) != 1 || accounts[0].Disabled)) {
			return VPNAccountsSnapshot{}, errors.New("VPN 专用账号文件内容不符合当前角色")
		}
	}
	revision := func() string { return digest([]byte(digest(config) + ":" + view.Path + ":" + digest(raw))) }
	if change != nil {
		if change.Revision != revision() {
			return VPNAccountsSnapshot{}, errVPNAccountsConflict
		}
		accounts, err = editVPNAccounts(role, accounts, change)
		if err != nil {
			return VPNAccountsSnapshot{}, err
		}
		next, err := tunnel.EncodeAccounts(accounts)
		if err != nil {
			return VPNAccountsSnapshot{}, err
		}
		var nextConfig string
		if configured == "" {
			edits := map[string]string{key: path}
			withField, err := prepareVPNConfigEdits(config, edits, role)
			if err != nil {
				return VPNAccountsSnapshot{}, err
			}
			nextConfig, err = updateConfigFields(withField, edits)
			if err != nil {
				return VPNAccountsSnapshot{}, err
			}
		}
		if exists {
			err = persistTunnelAccounts(view.Path, raw, next, func() error { return nil })
		} else {
			err = exclusiveFile(view.Path, next)
		}
		if err != nil {
			return VPNAccountsSnapshot{}, err
		}
		if nextConfig != "" {
			if err := persistTunnelAccounts(configPath, config, []byte(nextConfig), func() error { return nil }); err != nil {
				// Only remove the newly created file if it is still ours and
				// the configuration did not end up referencing it.
				current, configErr := regularRead(configPath, 1<<20)
				created, fileErr := regularRead(view.Path, 1<<20)
				if configErr == nil && fileErr == nil && digest(current) == digest(config) && digest(created) == digest(next) {
					_ = os.Remove(view.Path)
				}
				return VPNAccountsSnapshot{}, err
			}
			config = []byte(nextConfig)
			view.ConfigChanged = true
			view.ConfigValue = path
		}
		raw = next
	}
	view.Revision = revision()
	for _, account := range accounts {
		view.Accounts = append(view.Accounts, VPNAccountInfo{Username: account.Username, Disabled: account.Disabled})
	}
	return view, nil
}

func sameAccountFile(a, b string) bool {
	if a == b {
		return true
	}
	first, errA := os.Stat(a)
	second, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(first, second)
}

func editVPNAccounts(role string, accounts []tunnel.Account, change *VPNAccountChange) ([]tunnel.Account, error) {
	if role == "client" {
		if change.Action != "save" || change.Disabled != nil || change.NewUsername != "" {
			return nil, errors.New("客户端只能保存一组 VPN 凭据")
		}
		password := change.Password
		if password == "" && len(accounts) == 1 {
			password = accounts[0].Password
		}
		return []tunnel.Account{{Username: change.Username, Password: password}}, nil
	}
	if change.Action != "create" && change.Action != "update" && change.Action != "delete" {
		return nil, errors.New("VPN 账号操作无效")
	}
	if change.NewUsername != "" && change.NewUsername != change.Username {
		if change.Action != "update" || slices.ContainsFunc(accounts, func(a tunnel.Account) bool { return a.Username == change.NewUsername }) {
			return nil, errors.New("VPN 新用户名已存在或操作无效")
		}
	}
	next, err := editTunnelAccounts(accounts, &TunnelAccountChange{Action: change.Action, Username: change.Username, Password: change.Password, Disabled: change.Disabled})
	if err != nil {
		return nil, err
	}
	if change.NewUsername != "" {
		for i := range next {
			if next[i].Username == change.Username {
				next[i].Username = change.NewUsername
				break
			}
		}
	}
	return next, nil
}
