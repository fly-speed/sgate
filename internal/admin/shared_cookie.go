package admin

import (
	"net/http"
	"strings"
	"time"
)

// 旧版 Cookie 名包含已废弃的管理地址。只迁移能通过本地会话、账号版本和
// 有效期校验的令牌，不再读取旧端口，也不延长会话寿命或新增持久化文件。
func (h *Handler) migrateSharedCookie(w http.ResponseWriter, r *http.Request) {
	if !h.cfg.shared {
		return
	}
	if _, err := r.Cookie(h.cookie); err == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, old := range r.Cookies() {
		if !strings.HasPrefix(old.Name, "sgate_admin_") || len(old.Name) != len("sgate_admin_")+12 || !validToken(old.Value) {
			continue
		}
		candidate := r.Clone(r.Context())
		candidate.AddCookie(&http.Cookie{Name: h.cookie, Value: old.Value})
		session, err := h.readSession(candidate)
		if err != nil {
			continue
		}
		current := &http.Cookie{
			Name: h.cookie, Value: old.Value, Path: "/", HttpOnly: true,
			SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil,
			Expires: session.Expires, MaxAge: int(time.Until(session.Expires).Seconds()),
		}
		http.SetCookie(w, current)
		http.SetCookie(w, &http.Cookie{
			Name: old.Name, Path: "/", MaxAge: -1, HttpOnly: true,
			SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil,
		})
		r.AddCookie(current)
		return
	}
}
