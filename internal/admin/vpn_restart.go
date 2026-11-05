package admin

import (
	"net/http"
	"slices"
	"strconv"
	"strings"

	"sgate/internal/vpn"
)

func (h *Handler) vpnRestartFields(current []byte) []string {
	fields := []string{}
	var retry bool
	if h.view.VPNRestartPending != nil {
		pending, failed, err := h.view.VPNRestartPending()
		if err == nil && !pending {
			return fields
		}
		retry = failed
	}
	values := func(content []byte) map[string]string {
		fields, _ := configFields(string(content))
		out := make(map[string]string, len(fields))
		for _, field := range fields {
			out[field.Key] = field.Value
		}
		return out
	}
	old, saved := values(h.startupContent), values(current)
	for _, setting := range vpn.Settings(h.view.Role) {
		if slices.Contains(h.view.LiveFields, setting.Key) {
			continue
		}
		before, found := old[setting.Key]
		if !found {
			before = setting.Default
		}
		after, found := saved[setting.Key]
		if !found {
			after = setting.Default
		}
		if before != after || (h.vpnAccountRestartPending && setting.Key == VPNAccountFileKey(h.view.Role)) ||
			(retry && slices.Contains(h.vpnRestartAttemptFields, setting.Key)) {
			fields = append(fields, setting.Key)
		}
	}
	return fields
}

// mergeVPNBaseline preserves every non-VPN pending setting and updates only
// the saved VPN portion of the management restart baseline. No file is written.
func mergeVPNBaseline(baseline, current []byte) ([]byte, error) {
	fields, err := configFields(string(current))
	if err != nil {
		return nil, err
	}
	entries := []string{}
	for _, field := range fields {
		if strings.HasPrefix(field.Key, "vpn-") {
			entries = append(entries, "    "+field.Key+" = "+strconv.Quote(field.Value))
		}
	}
	lines := []string{}
	for _, line := range strings.Split(string(baseline), "\n") {
		key, _, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && strings.HasPrefix(strings.TrimSpace(key), "vpn-") {
			continue
		}
		if strings.TrimSpace(line) == "}" {
			lines = append(lines, entries...)
			entries = nil
		}
		lines = append(lines, line)
	}
	lines = append(lines, entries...)
	return []byte(strings.Join(lines, "\n")), nil
}
func (h *Handler) restartVPN(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Revision string `json:"revision"`
	}
	if !decode(w, r, &input, 4096) {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.authorizeMutation(w, r) {
		return
	}
	if h.view.RestartVPN == nil {
		problem(w, 404, "当前程序不支持单独重启 VPN，请更新程序。")
		return
	}
	current, err := regularRead(h.view.AppConfigPath, 1<<20)
	if err != nil {
		problem(w, 500, "无法读取 VPN 配置")
		return
	}
	if input.Revision == "" || input.Revision != h.configRevision(current) {
		problem(w, 409, "配置已变化，请重新加载后再重启 VPN。")
		return
	}
	fields := h.vpnRestartFields(current)
	if len(fields) == 0 {
		problem(w, 409, "没有待生效的 VPN 配置，请先保存需要重启的修改。")
		return
	}
	baseline, err := mergeVPNBaseline(h.startupContent, current)
	if err != nil {
		problem(w, 400, "VPN 配置格式无效")
		return
	}
	if err = h.view.RestartVPN(); err != nil {
		problem(w, 409, err.Error())
		return
	}
	after, readErr := regularRead(h.view.AppConfigPath, 1<<20)
	if readErr != nil || h.configRevision(after) != input.Revision {
		problem(w, 409, "VPN 已发起重启，但配置在操作期间发生变化，请重新加载核对。")
		return
	}
	h.vpnRestartAttemptFields = fields
	h.vpnAccountRestartPending = false
	h.startupContent = baseline
	h.startupRevision = h.restartDigest(baseline)
	var runtime any
	if h.view.Runtime != nil {
		runtime = h.view.Runtime()
	}
	pending := h.vpnRestartFields(current)
	reply(w, 200, map[string]any{"ok": true, "runtime": runtime, "vpn_restart_pending": len(pending) != 0, "vpn_restart_fields": pending, "requires_restart": h.restartDigest(current) != h.startupRevision || h.mappingRestartPending || h.clientKeyPending})
}
