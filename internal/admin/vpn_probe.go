package admin

import (
	"context"
	"net/http"
	"strings"
	"time"

	"sgate/internal/vpn"
)

func (h *Handler) testVPN(w http.ResponseWriter, r *http.Request) {
	if h.view.Role != "client" || h.view.TestVPN == nil {
		problem(w, 404, "当前服务不支持客户端 VPN 测试")
		return
	}
	var input struct {
		Fields map[string]string `json:"fields"`
	}
	if !decode(w, r, &input, 16<<10) {
		return
	}
	allowed := map[string]bool{"key-file": true, "tls-ca": true}
	for _, s := range vpn.Settings("client") {
		allowed[s.Key] = true
	}
	for key, value := range input.Fields {
		if !allowed[key] {
			problem(w, 400, "VPN 测试包含不支持的参数")
			return
		}
		if len(value) > 4095 || strings.ContainsAny(value, "\r\n\x00") {
			problem(w, 400, "VPN 测试参数过长或包含无效字符")
			return
		}
	}
	select {
	case h.probeGate <- struct{}{}:
		defer func() { <-h.probeGate }()
	default:
		problem(w, 429, "正在进行的连接测试过多，请稍后重试")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	result, err := h.view.TestVPN(ctx, input.Fields)
	if err != nil {
		problem(w, 400, "无法加载 VPN 测试配置，请检查客户端配置和日志")
		return
	}
	reply(w, 200, result)
}

func (h *Handler) validateServerVPN(w http.ResponseWriter, r *http.Request) {
	if h.view.Role != "server" || h.view.ValidateServerVPN == nil {
		problem(w, 404, "服务端 VPN 配置校验不可用")
		return
	}
	var input struct {
		Fields     map[string]string `json:"fields"`
		Revision   string            `json:"revision"`
		Parameters bool              `json:"parameters"`
	}
	if !decode(w, r, &input, 64<<10) {
		return
	}
	allowed := map[string]bool{}
	for _, s := range vpn.Settings("server") {
		allowed[s.Key] = true
	}
	for key, value := range input.Fields {
		if !allowed[key] || len(value) > 4095 || strings.ContainsAny(value, "\r\n\x00") {
			problem(w, 400, "VPN 校验包含不支持的参数")
			return
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.authorizeMutation(w, r) {
		return
	}
	current, err := regularRead(h.view.AppConfigPath, 1<<20)
	if err != nil {
		problem(w, 500, "无法读取代理配置")
		return
	}
	if input.Revision == "" || input.Revision != h.configRevision(current) {
		problem(w, 409, "配置已被其他操作修改，请重新加载后再编辑")
		return
	}
	if err := h.view.ValidateServerVPN(input.Fields, input.Parameters); err != nil {
		h.configValidationProblem(w, err, string(current))
		return
	}
	reply(w, 200, map[string]bool{"ok": true})
}
