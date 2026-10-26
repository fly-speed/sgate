package admin

import (
	"context"
	"net/http"
	"time"
)

func (h *Handler) testProxy(w http.ResponseWriter, r *http.Request) {
	if h.view.Role != "client" || h.view.TestProxy == nil {
		problem(w, http.StatusNotFound, "当前服务不支持映射测试")
		return
	}
	var mapping ProxyMapping
	if !decode(w, r, &mapping, 8192) {
		return
	}
	mappings := []ProxyMapping{mapping}
	if _, err := encodeProxyMappings(mappings); err != nil {
		problem(w, http.StatusBadRequest, err.Error())
		return
	}
	select {
	case h.probeGate <- struct{}{}:
		defer func() { <-h.probeGate }()
	default:
		problem(w, http.StatusTooManyRequests, "正在进行的映射测试过多，请稍后重试")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	result, err := h.view.TestProxy(ctx, mappings[0])
	if err != nil {
		// A config-loading error may contain secrets; do not expose raw flags.
		problem(w, http.StatusBadRequest, "无法加载生效的代理参数，请检查客户端配置和日志")
		return
	}
	reply(w, http.StatusOK, result)
}
