package admin

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const secretMarker = "__SGATE_KEEP_EXISTING_PASSWORD__"

func validateWebDir(dir string) error {
	for _, name := range []string{"index.html", "app.css", "app.js"} {
		if _, e := regularRead(filepath.Join(dir, name), 1<<20); e != nil {
			return fmt.Errorf("management web-dir requires readable regular files: %s: %w", filepath.Join(dir, name), e)
		}
	}
	return nil
}

func reply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func problem(w http.ResponseWriter, status int, message string) {
	reply(w, status, map[string]string{"error": message})
}
func decode(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	media, _, e := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if e != nil || media != "application/json" {
		problem(w, 415, "需要 application/json 请求")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if e = d.Decode(v); e != nil {
		problem(w, 400, "请求格式无效或过大")
		return false
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		problem(w, 400, "请求只能包含一个 JSON 对象")
		return false
	}
	return true
}
func sameOrigin(r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, e := url.Parse(origin)
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		return e == nil && u.Host == r.Host && u.Scheme == scheme && u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == ""
	}
	return true
}
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	if r.URL.Path == "/" || r.URL.Path == "/assets/app.css" || r.URL.Path == "/assets/app.js" {
		if r.Method != "GET" {
			w.WriteHeader(405)
			return
		}
		name := "index.html"
		typ := "text/html; charset=utf-8"
		if r.URL.Path != "/" {
			name = strings.TrimPrefix(r.URL.Path, "/assets/")
			if name == "app.css" {
				typ = "text/css; charset=utf-8"
			} else {
				typ = "text/javascript; charset=utf-8"
			}
		}
		// Read from disk on every request so interface updates need no rebuild
		// or proxy restart. Only these explicit routes expose files.
		b, e := regularRead(filepath.Join(h.cfg.WebDir, name), 1<<20)
		if e != nil {
			problem(w, 503, "管理界面文件不可读取，请检查 web-dir")
			return
		}
		w.Header().Set("Content-Type", typ)
		_, _ = w.Write(b)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		http.NotFound(w, r)
		return
	}
	if !sameOrigin(r) {
		problem(w, 403, "请求来源不允许")
		return
	}
	if r.URL.Path == "/api/status" && r.Method == "GET" {
		h.mu.Lock()
		defer h.mu.Unlock()
		_, ae := loadAccount(h.cfg.UsersFile)
		s, e := h.readSession(r)
		v := map[string]any{"setup_required": os.IsNotExist(ae), "authenticated": e == nil, "role": h.view.Role, "session_timeout_seconds": h.cfg.SessionTimeout.Seconds(), "setup_token_file": filepath.Base(h.cfg.SetupTokenFile)}
		if e == nil {
			v["username"], v["csrf"], v["expires"] = s.Username, s.CSRF, s.Expires
		}
		reply(w, 200, v)
		return
	}
	if (r.URL.Path == "/api/login" || r.URL.Path == "/api/setup") && r.Method == "POST" {
		h.authenticate(w, r)
		return
	}
	h.mu.Lock()
	s, e := h.readSession(r)
	h.mu.Unlock()
	if e != nil {
		problem(w, 401, "登录已过期，请重新登录")
		return
	}
	if r.Method != "GET" && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(s.CSRF)) != 1 {
		problem(w, 403, "请求验证失败，请刷新页面")
		return
	}
	switch {
	case r.URL.Path == "/api/stats" && r.Method == "GET":
		reply(w, 200, h.view.Stats.Snapshot())
	case r.URL.Path == "/api/config" && r.Method == "GET":
		h.getConfig(w)
	case r.URL.Path == "/api/config" && r.Method == "PUT":
		h.putConfig(w, r)
	case r.URL.Path == "/api/apply" && r.Method == "POST":
		h.mu.Lock()
		defer h.mu.Unlock()
		if !h.authorizeMutation(w, r) {
			return
		}
		if h.view.Apply == nil {
			problem(w, 503, "代理重启不可用")
			return
		}
		if e := h.view.Apply(); e != nil {
			problem(w, 409, e.Error())
			return
		}
		b, e := regularRead(h.view.AppConfigPath, 1<<20)
		if e == nil {
			h.startupRevision = digest(b)
		}
		reply(w, 200, map[string]any{"applied": true})
	case r.URL.Path == "/api/logout" && r.Method == "POST":
		h.mu.Lock()
		defer h.mu.Unlock()
		if !h.authorizeMutation(w, r) {
			return
		}
		if c, e := r.Cookie(h.cookie); e == nil {
			if e = os.Remove(h.sessionPath(c.Value)); e != nil && !os.IsNotExist(e) {
				problem(w, 500, "退出登录失败")
				return
			}
		}
		h.clearCookie(w, r)
		reply(w, 200, map[string]bool{"ok": true})
	case r.URL.Path == "/api/account" && r.Method == "PUT":
		h.changeAccount(w, r, s)
	default:
		problem(w, 405, "请求方法或接口不存在")
	}
}

// Call with mu held immediately before a mutation. Reading a request body or
// waiting for another apply can outlive the session checked by ServeHTTP.
func (h *Handler) authorizeMutation(w http.ResponseWriter, r *http.Request) bool {
	s, e := h.readSession(r)
	if e != nil {
		problem(w, 401, "登录已过期，请重新登录")
		return false
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(s.CSRF)) != 1 {
		problem(w, 403, "请求验证失败，请刷新页面")
		return false
	}
	return true
}
func (h *Handler) authenticate(w http.ResponseWriter, r *http.Request) {
	if !h.allowAttempt(r) {
		problem(w, 429, "尝试次数过多，请五分钟后再试")
		return
	}
	select {
	case h.authGate <- struct{}{}:
		defer func() { <-h.authGate }()
	default:
		problem(w, 429, "正在处理登录，请稍后重试")
		return
	}
	var input struct {
		Username   string `json:"username"`
		Password   string `json:"password"`
		SetupToken string `json:"setup_token"`
	}
	if !decode(w, r, &input, 8192) {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	a, e := loadAccount(h.cfg.UsersFile)
	if r.URL.Path == "/api/setup" {
		if !os.IsNotExist(e) {
			problem(w, 409, "管理员已创建或账号文件不可读取")
			return
		}
		token, e := regularRead(h.cfg.SetupTokenFile, 256)
		if e != nil || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(string(token))), []byte(input.SetupToken)) != 1 {
			problem(w, 403, "初始化令牌无效")
			return
		}
		a, e = makeAccount(strings.TrimSpace(input.Username), input.Password)
		if e != nil {
			problem(w, 400, e.Error())
			return
		}
		b, _ := json.MarshalIndent(a, "", "  ")
		if e = exclusiveFile(h.cfg.UsersFile, b); e != nil {
			problem(w, 409, "账号创建失败，可能已由另一个请求创建")
			return
		}
		_ = os.Remove(h.cfg.SetupTokenFile)
	} else if e != nil || !a.verify(input.Username, input.Password) {
		problem(w, 401, "用户名或密码不正确")
		return
	}
	if e = h.newSession(w, r, a); e != nil {
		problem(w, 500, "无法保存登录会话："+e.Error())
	}
}
func (h *Handler) clearCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: h.cookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode})
}
func (h *Handler) changeAccount(w http.ResponseWriter, r *http.Request, s loginSession) {
	select {
	case h.authGate <- struct{}{}:
		defer func() { <-h.authGate }()
	default:
		problem(w, 429, "正在处理账号请求，请稍后重试")
		return
	}
	var input struct {
		Username        string `json:"username"`
		Password        string `json:"password"`
		CurrentPassword string `json:"current_password"`
	}
	if !decode(w, r, &input, 8192) {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.authorizeMutation(w, r) {
		return
	}
	a, e := loadAccount(h.cfg.UsersFile)
	if e != nil || !a.verify(s.Username, input.CurrentPassword) {
		problem(w, 403, "当前密码不正确")
		return
	}
	next, e := makeAccount(strings.TrimSpace(input.Username), input.Password)
	if e != nil {
		problem(w, 400, e.Error())
		return
	}
	b, _ := json.MarshalIndent(next, "", "  ")
	if e = atomicFile(h.cfg.UsersFile, b); e != nil {
		problem(w, 500, "账号保存失败")
		return
	}
	// The credential revision invalidates every existing session, including this one.
	files, _ := os.ReadDir(h.cfg.SessionDir)
	for _, f := range files {
		if sessionFile(f.Name()) {
			_ = os.Remove(filepath.Join(h.cfg.SessionDir, f.Name()))
		}
	}
	h.clearCookie(w, r)
	reply(w, 200, map[string]bool{"login_required": true})
}
func secretConfigKey(line string) string {
	key, _, ok := strings.Cut(line, "=")
	if !ok {
		return ""
	}
	key = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(key), "\uFEFF"))
	switch key {
	case "password", "master_args", "master_env":
		return key
	}
	return ""
}
func redactConfig(b []byte) string {
	lines := strings.Split(string(b), "\n")
	for i, line := range lines {
		if secretConfigKey(line) != "" {
			key, _, _ := strings.Cut(line, "=")
			lines[i] = key + "= " + strconv.Quote(secretMarker)
		}
	}
	return strings.Join(lines, "\n")
}
func restoreSecret(content string, current []byte) (string, error) {
	saved := make(map[string]string)
	for _, line := range strings.Split(string(current), "\n") {
		if key := secretConfigKey(line); key != "" {
			saved[key] = line
		}
	}
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		if key := secretConfigKey(line); key != "" {
			_, value, _ := strings.Cut(line, "=")
			if value = strings.TrimSpace(value); value == strconv.Quote(secretMarker) || value == secretMarker {
				if saved[key] == "" {
					return "", errors.New("没有可保留的密码，请填写代理密码")
				}
				lines[i] = saved[key]
			}
		}
	}
	return strings.Join(lines, "\n"), nil
}
func (h *Handler) getConfig(w http.ResponseWriter) {
	h.mu.Lock()
	defer h.mu.Unlock()
	b, e := regularRead(h.view.AppConfigPath, 1<<20)
	if e != nil {
		problem(w, 500, "无法读取代理配置")
		return
	}
	v := map[string]any{"content": redactConfig(b), "revision": digest(b), "requires_restart": digest(b) != h.startupRevision, "path": h.view.AppConfigPath, "role": h.view.Role, "password_placeholder": secretMarker}
	if h.view.Runtime != nil {
		v["runtime"] = h.view.Runtime()
	}
	reply(w, 200, v)
}
func (h *Handler) putConfig(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Content  string `json:"content"`
		Revision string `json:"revision"`
		Apply    bool   `json:"apply"`
	}
	if !decode(w, r, &input, 2<<20) {
		return
	}
	if len(input.Content) > 1<<20 {
		problem(w, 413, "配置文件不能超过 1 MiB")
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.authorizeMutation(w, r) {
		return
	}
	current, e := regularRead(h.view.AppConfigPath, 1<<20)
	if e != nil {
		problem(w, 500, "无法读取代理配置")
		return
	}
	if input.Revision == "" || input.Revision != digest(current) {
		problem(w, 409, "配置已被其他操作修改，请重新加载后再编辑")
		return
	}
	content, e := restoreSecret(input.Content, current)
	if e != nil {
		problem(w, 400, e.Error())
		return
	}
	f, e := os.CreateTemp(filepath.Dir(h.view.AppConfigPath), ".sgate-validate-*")
	if e != nil {
		problem(w, 500, "无法准备配置校验")
		return
	}
	name := f.Name()
	defer os.Remove(name)
	if _, e = f.WriteString(content); e == nil {
		e = f.Sync()
	}
	if ce := f.Close(); e == nil {
		e = ce
	}
	if e != nil {
		problem(w, 500, "无法写入待校验配置")
		return
	}
	if e = h.view.Validate(name); e != nil {
		problem(w, 400, "配置校验失败："+e.Error())
		return
	}
	// A validator may perform slow certificate/key reads. Detect external edits
	// made during that work before committing the candidate.
	latest, readErr := regularRead(h.view.AppConfigPath, 1<<20)
	if readErr != nil || digest(latest) != input.Revision {
		problem(w, 409, "校验期间配置发生变化，请重新加载后再编辑")
		return
	}
	if e = atomicFile(h.view.AppConfigPath+".bak", current); e != nil {
		problem(w, 500, "无法保存配置备份")
		return
	}
	if e = atomicFile(h.view.AppConfigPath, []byte(content)); e != nil {
		problem(w, 500, "配置保存失败")
		return
	}
	if input.Apply {
		if h.view.Apply == nil {
			problem(w, 503, "配置已保存，但代理重启不可用")
			return
		}
		if e = h.view.Apply(); e != nil {
			if restoreErr := atomicFile(h.view.AppConfigPath, current); restoreErr != nil {
				problem(w, 500, "重启失败，且无法恢复配置文件："+e.Error())
				return
			}
			problem(w, 409, e.Error())
			return
		}
		h.startupRevision = digest([]byte(content))
	}
	reply(w, 200, map[string]any{"revision": digest([]byte(content)), "requires_restart": !input.Apply, "applied": input.Apply})
}
func exclusiveFile(path string, data []byte) error {
	f, e := os.CreateTemp(filepath.Dir(path), ".sgate-account-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(data)
	}
	if e == nil {
		e = f.Sync()
	}
	if ce := f.Close(); e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	// Linking the complete file is atomic and cannot overwrite a concurrent setup.
	if e = os.Link(f.Name(), path); e != nil {
		return e
	}
	dir, e := os.Open(filepath.Dir(path))
	if e != nil {
		return e
	}
	defer dir.Close()
	return dir.Sync()
}
