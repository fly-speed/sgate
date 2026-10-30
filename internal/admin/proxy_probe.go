package admin

import (
	"context"
	"log"
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
	h.mu.Lock()
	previous, _, readErr := h.savedMappingsMustRead()
	var mappings []ProxyMapping
	if readErr == nil {
		mappings, readErr = restoreMappingPasswords([]ProxyMapping{mapping}, previous)
	}
	h.mu.Unlock()
	if readErr != nil {
		problem(w, 400, readErr.Error())
		return
	}
	if _, err := encodeProxyMappings(mappings); err != nil {
		log.Printf("mapping connectivity test rejected: stage=validation reason=%q", err.Error())
		problem(w, http.StatusBadRequest, err.Error())
		return
	}
	select {
	case h.probeGate <- struct{}{}:
		defer func() { <-h.probeGate }()
	default:
		log.Printf("mapping connectivity test rejected: listen=%q reason=too many concurrent tests", mappings[0].Listen)
		problem(w, http.StatusTooManyRequests, "正在进行的映射测试过多，请稍后重试")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	result, err := h.view.TestProxy(ctx, mappings[0])
	if err != nil {
		// A config-loading error may contain secrets; do not expose raw flags.
		log.Printf("mapping connectivity test failed: listen=%q stage=config reason=unable to load effective client settings", mappings[0].Listen)
		problem(w, http.StatusBadRequest, "无法加载生效的代理参数，请检查客户端配置和日志")
		return
	}
	if !result.OK {
		if result.Detail == "" {
			result.Detail = "connectivity test failed; check client and server logs"
		}
		if result.Error == "" {
			result.Error = result.Detail
		}
		log.Printf("mapping connectivity test failed: test_id=%s listen=%q target=%q stage=%s error=%q",
			result.TestID, mappings[0].Listen, result.Target, result.Stage, result.Error)
	}
	reply(w, http.StatusOK, result)
}
