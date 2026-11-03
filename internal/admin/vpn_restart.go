package admin

import (
	"net/http"
	"strconv"
	"strings"
)

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
	h.startupContent = baseline
	h.startupRevision = h.restartDigest(baseline)
	var runtime any
	if h.view.Runtime != nil {
		runtime = h.view.Runtime()
	}
	reply(w, 200, map[string]any{"ok": true, "runtime": runtime, "requires_restart": h.restartDigest(current) != h.startupRevision || h.mappingRestartPending})
}
