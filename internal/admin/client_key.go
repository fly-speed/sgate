package admin

import (
	"encoding/hex"
	"net/http"
	"strings"

	"sgate/internal/tunnel"
)

func (h *Handler) clientKey(w http.ResponseWriter, r *http.Request) {
	if h.view.Role != "client" || h.view.ClientKeyPath == nil {
		problem(w, 404, "仅客户端支持修改共享密钥文件")
		return
	}
	var input struct {
		Revision string `json:"revision"`
		Key      string `json:"key"`
	}
	if r.Method == http.MethodPut && !decode(w, r, &input, 1024) {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, err := h.readSession(r); err != nil {
		problem(w, 401, "登录已过期，请重新登录")
		return
	}
	path, err := h.view.ClientKeyPath()
	if err != nil {
		problem(w, 400, err.Error())
		return
	}
	raw, err := regularRead(path, 1024)
	if err != nil {
		problem(w, 400, "无法读取客户端共享密钥文件")
		return
	}
	defer clear(raw)
	current, err := tunnel.ParseKey(strings.TrimSpace(string(raw)))
	clear(current)
	if err != nil {
		problem(w, 400, "客户端共享密钥文件格式无效")
		return
	}
	revision := digest([]byte(path + ":" + digest(raw)))
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodGet {
		reply(w, 200, map[string]string{"path": path, "revision": revision})
		return
	}
	if input.Revision != revision {
		problem(w, 409, "共享密钥文件已变化，请重新打开编辑")
		return
	}
	key, err := tunnel.ParseKey(strings.TrimSpace(input.Key))
	if err != nil {
		problem(w, 400, "共享密钥必须是 64 个十六进制字符")
		return
	}
	defer clear(key)
	next := append(hex.AppendEncode(nil, key), '\n')
	defer clear(next)
	if err := atomicFile(path, next); err != nil {
		problem(w, 500, "无法保存客户端共享密钥文件")
		return
	}
	h.credentialFilesPending = true
	reply(w, 200, map[string]bool{"ok": true})
}
