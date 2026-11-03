package admin

import "net/http"

func (h *Handler) controlVPN(w http.ResponseWriter, r *http.Request) {
	if h.view.Role != "client" || h.view.ControlVPN == nil {
		problem(w, 404, "仅客户端支持手动控制 VPN")
		return
	}
	if err := h.view.ControlVPN(r.URL.Path == "/api/vpn-start"); err != nil {
		problem(w, 400, err.Error())
		return
	}
	var runtime any
	if h.view.Runtime != nil {
		runtime = h.view.Runtime()
	}
	reply(w, 200, map[string]any{"ok": true, "runtime": runtime})
}
