package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/pbkdf2"
)

const iterations = 600000
const sessionPrefix = "sgate-session-"

func sessionFile(name string) bool {
	return strings.HasPrefix(name, sessionPrefix) && strings.HasSuffix(name, ".json") &&
		validToken(strings.TrimSuffix(strings.TrimPrefix(name, sessionPrefix), ".json"))
}

type account struct {
	Username   string `json:"username"`
	Salt       string `json:"salt"`
	Hash       string `json:"password_hash"`
	Iterations int    `json:"iterations"`
}
type loginSession struct {
	Username string    `json:"username"`
	Revision string    `json:"account_revision"`
	CSRF     string    `json:"csrf"`
	Created  time.Time `json:"created"`
	Expires  time.Time `json:"expires"`
}
type attempt struct {
	Count int
	Since time.Time
}

// Handler 在代理热重启期间继续存在，登录身份由持久会话文件恢复。
// mu 串行化账号、会话和配置文件操作；authGate 限制同时进行的密码派生计算。
type Handler struct {
	cfg                      Config
	view                     View
	cookie                   string
	startupRevision          string
	startupContent           []byte
	mappingRestartPending    bool
	vpnRestartAttemptFields  []string
	vpnAccountRestartPending bool
	mu                       sync.Mutex
	attempts                 map[string]attempt
	authGate                 chan struct{}
	probeGate                chan struct{}
}

func randomToken() (string, error) {
	var b [32]byte
	_, e := rand.Read(b[:])
	return hex.EncodeToString(b[:]), e
}
func digest(b []byte) string {
	v := sha256.Sum256(b)
	return hex.EncodeToString(v[:])
}
func validToken(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 32 && s == strings.ToLower(s)
}
func validName(s string) bool {
	n := utf8.RuneCountInString(s)
	if n < 3 || n > 64 {
		return false
	}
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("_.@-", r) {
			return false
		}
	}
	return true
}
func makeAccount(name, password string) (account, error) {
	if !validName(name) || utf8.RuneCountInString(password) < 12 || len(password) > 256 {
		return account{}, errors.New("用户名需为 3～64 位字母、数字或 _ . @ -；密码至少 12 个字符、最多 256 字节")
	}
	salt := make([]byte, 16)
	if _, e := rand.Read(salt); e != nil {
		return account{}, e
	}
	key := pbkdf2.Key([]byte(password), salt, iterations, 32, sha256.New)
	return account{
		Username:   name,
		Salt:       hex.EncodeToString(salt),
		Hash:       hex.EncodeToString(key),
		Iterations: iterations,
	}, nil
}
func loadAccount(path string) (account, error) {
	b, e := regularRead(path, 4096)
	if e != nil {
		return account{}, e
	}
	var a account
	if e = json.Unmarshal(b, &a); e != nil {
		return a, errors.New("invalid management account file")
	}
	salt, se := hex.DecodeString(a.Salt)
	key, ke := hex.DecodeString(a.Hash)
	if !validName(a.Username) || se != nil || ke != nil || len(salt) != 16 || len(key) != 32 ||
		a.Iterations < iterations ||
		a.Iterations > 2000000 {
		return a, errors.New("invalid management account fields")
	}
	return a, nil
}
func (a account) verify(name, password string) bool {
	if len(password) > 256 {
		return false
	}
	salt, _ := hex.DecodeString(a.Salt)
	want, _ := hex.DecodeString(a.Hash)
	got := pbkdf2.Key([]byte(password), salt, a.Iterations, 32, sha256.New)
	return subtle.ConstantTimeCompare(got, want) == 1 && name == a.Username
}
func (a account) revision() string {
	b, _ := json.Marshal(a)
	return digest(b)
}
func New(c Config, v View) (*Handler, error) {
	if e := c.validate(); e != nil {
		return nil, e
	}
	if e := validateWebDir(c.WebDir); e != nil {
		return nil, e
	}
	if v.AppConfigPath == "" || v.Stats == nil || v.Validate == nil {
		return nil, errors.New("management requires -config, statistics and a validator")
	}
	for _, path := range []string{c.UsersFile, c.SessionDir, c.SetupTokenFile} {
		a, _ := filepath.Abs(path)
		b, _ := filepath.Abs(v.AppConfigPath)
		if a == b {
			return nil, errors.New("management storage must be separate from application configuration")
		}
	}
	if e := os.MkdirAll(filepath.Dir(c.UsersFile), 0700); e != nil {
		return nil, e
	}
	if e := os.MkdirAll(filepath.Dir(c.SetupTokenFile), 0700); e != nil {
		return nil, e
	}
	if e := os.MkdirAll(c.SessionDir, 0700); e != nil {
		return nil, e
	}
	st, e := os.Lstat(c.SessionDir)
	if e != nil || !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return nil, errors.New("session-dir must be a private directory (mode 0700)")
	}
	h := &Handler{cfg: c, view: v, attempts: make(map[string]attempt), authGate: make(chan struct{}, 1), probeGate: make(chan struct{}, 4)}
	path, _ := filepath.Abs(c.UsersFile)
	identity := path + c.Listen
	if c.shared {
		directory, _ := filepath.Abs(c.SessionDir)
		identity = path + "\x00shared\x00" + directory
	}
	h.cookie = "sgate_admin_" + digest([]byte(identity))[:12]
	if b, e := regularRead(v.AppConfigPath, 1<<20); e == nil {
		h.startupRevision = h.restartDigest(b)
		h.startupContent = append([]byte(nil), b...)
	} else {
		return nil, e
	}
	if _, e := loadAccount(c.UsersFile); os.IsNotExist(e) {
		token, e := randomToken()
		if e != nil {
			return nil, e
		}
		f, e := os.OpenFile(c.SetupTokenFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e == nil {
			_, e = f.WriteString(token + "\n")
			if e == nil {
				e = f.Sync()
			}
			ce := f.Close()
			if e == nil {
				e = ce
			}
			if e != nil {
				return nil, e
			}
		} else if !os.IsExist(e) {
			return nil, e
		}
		b, e := regularRead(c.SetupTokenFile, 256)
		if e != nil || !validToken(strings.TrimSpace(string(b))) {
			return nil, errors.New("invalid first-account setup token file")
		}
		log.Printf("management first account: read initialization token from %s", c.SetupTokenFile)
	} else if e != nil {
		return nil, e
	}
	h.mu.Lock()
	e = h.cleanSessions(time.Now())
	h.mu.Unlock()
	if e != nil {
		return nil, e
	}
	return h, nil
}
func (h *Handler) sessionPath(token string) string {
	return filepath.Join(h.cfg.SessionDir, sessionPrefix+digest([]byte(token))+".json")
}
func (h *Handler) readSession(r *http.Request) (loginSession, error) {
	var s loginSession
	c, e := r.Cookie(h.cookie)
	if e != nil || !validToken(c.Value) {
		return s, errors.New("unauthorized")
	}
	path := h.sessionPath(c.Value)
	b, e := regularRead(path, 8192)
	if e != nil {
		return s, e
	}
	if e = json.Unmarshal(b, &s); e != nil {
		return s, e
	}
	a, e := loadAccount(h.cfg.UsersFile)
	now := time.Now()
	if e != nil || s.Username != a.Username || s.Revision != a.revision() || !validToken(s.CSRF) ||
		s.Created.After(now) ||
		!now.Before(s.Expires) ||
		!now.Before(s.Created.Add(h.cfg.SessionTimeout)) {
		_ = os.Remove(path)
		return s, errors.New("expired session")
	}
	return s, nil
}
func (h *Handler) newSession(w http.ResponseWriter, r *http.Request, a account) error {
	now := time.Now()
	if e := h.cleanSessions(now); e != nil {
		return e
	}
	files, e := os.ReadDir(h.cfg.SessionDir)
	if e != nil {
		return e
	}
	n := 0
	// Reauthentication replaces the caller's session, so it needs no extra
	// slot. Keep that old session until the replacement is safely persisted.
	oldPath := ""
	if _, err := h.readSession(r); err == nil {
		old, _ := r.Cookie(h.cookie)
		oldPath = h.sessionPath(old.Value)
	}
	for _, f := range files {
		if sessionFile(f.Name()) && filepath.Join(h.cfg.SessionDir, f.Name()) != oldPath {
			n++
		}
	}
	if n >= h.cfg.MaxSessions {
		return errors.New("登录会话数量已满，请稍后重试")
	}
	token, e := randomToken()
	if e != nil {
		return e
	}
	csrf, e := randomToken()
	if e != nil {
		return e
	}
	s := loginSession{
		Username: a.Username,
		Revision: a.revision(),
		CSRF:     csrf,
		Created:  now,
		Expires:  now.Add(h.cfg.SessionTimeout),
	}
	b, _ := json.Marshal(s)
	if e = atomicFile(h.sessionPath(token), b); e != nil {
		return e
	}
	if old, e := r.Cookie(h.cookie); e == nil && validToken(old.Value) {
		_ = os.Remove(h.sessionPath(old.Value))
	}
	http.SetCookie(
		w,
		&http.Cookie{
			Name:     h.cookie,
			Value:    token,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
			Secure:   r.TLS != nil,
			Expires:  s.Expires,
			MaxAge:   int(h.cfg.SessionTimeout.Seconds()),
		},
	)
	reply(w, 200, map[string]any{"username": a.Username, "csrf": csrf, "expires": s.Expires})
	return nil
}
func (h *Handler) cleanSessions(now time.Time) error {
	account, accountErr := loadAccount(h.cfg.UsersFile)
	revision := ""
	if accountErr == nil {
		revision = account.revision()
	}
	entries, e := os.ReadDir(h.cfg.SessionDir)
	if e != nil {
		return e
	}
	for _, entry := range entries {
		name := entry.Name()
		if !sessionFile(name) {
			continue
		}
		p := filepath.Join(h.cfg.SessionDir, name)
		b, e := regularRead(p, 8192)
		var s loginSession
		if e != nil || json.Unmarshal(b, &s) != nil || s.Revision != revision || !now.Before(s.Expires) ||
			!now.Before(s.Created.Add(h.cfg.SessionTimeout)) {
			if e := os.Remove(p); e != nil && !os.IsNotExist(e) {
				return e
			}
		}
	}
	return nil
}
func (h *Handler) reap(ctx context.Context) {
	interval := h.cfg.SessionTimeout / 2
	if interval > time.Minute {
		interval = time.Minute
	}
	if interval < time.Second {
		interval = time.Second
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			h.mu.Lock()
			e := h.cleanSessions(now)
			h.mu.Unlock()
			if e != nil {
				log.Printf("management session cleanup: %v", e)
			}
		}
	}
}
func (h *Handler) allowAttempt(r *http.Request) bool {
	host, _, e := net.SplitHostPort(r.RemoteAddr)
	if e != nil {
		host = r.RemoteAddr
	}
	now := time.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	for ip, a := range h.attempts {
		if now.Sub(a.Since) > 5*time.Minute {
			delete(h.attempts, ip)
		}
	}
	a, ok := h.attempts[host]
	if !ok {
		if len(h.attempts) >= 1024 {
			return false
		}
		a.Since = now
	}
	a.Count++
	h.attempts[host] = a
	return a.Count <= 20
}
