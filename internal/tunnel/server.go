package tunnel

import (
	"context"
	"crypto/hmac"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type ServerConfig struct {
	Key            []byte
	Password       string
	Target         string
	AllowedTargets []string
	Path           string
	SessionTTL     time.Duration
	MaxSessions    int
	Options        Options
}
type serverEntry struct {
	session *session
	inUse   bool
	idle    time.Time
}

// Server.mu 保护会话索引、nonce 防重放表和占用标志。
// 业务流与确认序列属于各自 session，使用 session.mu 单独保护。
type Server struct {
	cfg            ServerConfig
	ctx            context.Context
	cancel         context.CancelFunc
	mu             sync.Mutex
	sessions       map[string]*serverEntry
	nonces         map[string]time.Time
	closed         bool
	allowedTargets map[string]struct{}
	probeGate      chan struct{}
}

func NewServer(cfg ServerConfig) (*Server, error) {
	if len(cfg.Key) != 32 {
		return nil, errors.New("server key must contain 32 bytes")
	}
	if err := validatePassword(cfg.Password); err != nil {
		return nil, err
	}
	if _, _, err := net.SplitHostPort(cfg.Target); err != nil {
		return nil, errors.New("target must be host:port")
	}
	allowedTargets := make(map[string]struct{}, len(cfg.AllowedTargets))
	for _, target := range cfg.AllowedTargets {
		if err := validateSelectableTarget(target); err != nil {
			return nil, err
		}
		allowedTargets[target] = struct{}{}
	}
	cfg.AllowedTargets = append([]string(nil), cfg.AllowedTargets...)
	if cfg.Path == "" {
		cfg.Path = "/tunnel"
	}
	if cfg.Path[0] != '/' {
		return nil, errors.New("WebSocket path must start with /")
	}
	if cfg.SessionTTL == 0 {
		cfg.SessionTTL = 2 * time.Minute
	}
	if cfg.MaxSessions == 0 {
		cfg.MaxSessions = 128
	}
	if cfg.SessionTTL <= 0 || cfg.MaxSessions < 1 {
		return nil, errors.New("invalid session TTL or limit")
	}
	var err error
	cfg.Options, err = cfg.Options.defaults()
	if err != nil {
		return nil, err
	}
	cfg.Key = append([]byte(nil), cfg.Key...)
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		cfg:            cfg,
		ctx:            ctx,
		cancel:         cancel,
		sessions:       make(map[string]*serverEntry),
		nonces:         make(map[string]time.Time),
		allowedTargets: allowedTargets,
		probeGate:      make(chan struct{}, 4),
	}
	go s.reap()
	return s, nil
}
func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	s.cancel()
	for _, e := range s.sessions {
		e.session.close()
	}
	s.sessions = make(map[string]*serverEntry)
	s.nonces = nil
}
func (s *Server) reapLocked(now time.Time) {
	for id, e := range s.sessions {
		if !e.inUse && now.Sub(e.idle) > s.cfg.SessionTTL {
			e.session.close()
			delete(s.sessions, id)
		}
	}
	for n, t := range s.nonces {
		if now.Sub(t) > 2*time.Minute {
			delete(s.nonces, n)
		}
	}
}
func (s *Server) reap() {
	interval := s.cfg.SessionTTL / 2
	if interval <= 0 {
		interval = time.Nanosecond
	}
	if interval > time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case now := <-ticker.C:
			s.mu.Lock()
			s.reapLocked(now)
			s.mu.Unlock()
		}
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != s.cfg.Path {
		if r.Method == http.MethodGet && r.URL.Path == "/" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte("<!doctype html><html><title>Service</title><body>Service is running.</body></html>"))
			return
		}
		http.NotFound(w, r)
		return
	}
	request, ok := s.authenticateRequest(r)
	if !ok {
		id := r.Header.Get("X-Sgate-Session")
		if !validID(id) {
			id = ""
		}
		s.cfg.Options.Logf("tunnel handshake rejected: session=%s peer=%q reason=invalid authentication or handshake fields", id, r.RemoteAddr)
		http.NotFound(w, r)
		return
	}
	request.target, ok = s.selectTarget(w, r)
	if !ok {
		return
	}
	if values, probe := r.URL.Query()[probeQuery]; probe {
		if len(values) != 1 || values[0] != "1" {
			s.rejectHandshake(w, r, request, "invalid connectivity test", http.StatusBadRequest)
			return
		}
		s.serveProbe(w, r, request)
		return
	}
	entry, resumeDeadline, ok := s.acquireSession(w, r, request)
	if !ok {
		return
	}
	id, nonce := request.id, request.nonce
	confirmed := false
	defer func() {
		s.mu.Lock()
		entry.inUse = false
		// A failed upgrade or an abandoned resume must not renew an old session.
		if confirmed {
			entry.idle = time.Now()
		}
		s.mu.Unlock()
	}()
	serverNonce := randomID()
	headers := s.handshakeHeaders(request, serverNonce)
	upgrader := websocket.Upgrader{
		HandshakeTimeout: s.cfg.Options.DialTimeout,
		ReadBufferSize:   chunkSize,
		WriteBufferSize:  chunkSize,
	}
	ws, err := upgrader.Upgrade(w, r, headers)
	if err != nil {
		entry.session.opts.Logf("WebSocket upgrade failed: peer=%q reason=%v", r.RemoteAddr, err)
		return
	}
	entry.session.opts.Logf("tunnel connected: peer=%q", r.RemoteAddr)
	crypt := newCodec(s.cfg.Key, id, nonce, serverNonce, false)
	crypt.peerWindow = request.peerWindow
	err = entry.session.transportConfirmed(
		ws,
		crypt,
		resumeDeadline,
		func() { confirmed = true },
	)
	entry.session.opts.Logf("tunnel disconnected: peer=%q reason=%v", r.RemoteAddr, err)
}

// tunnelRequest 是通过请求头完整性和密码校验后才能使用的握手信息。
type tunnelRequest struct {
	id, nonce, timestamp, resume, credential string
	target                                   string
	peerWindow                               int
	reportStreamErrors                       bool
}

func (s *Server) authenticateRequest(r *http.Request) (tunnelRequest, bool) {
	id := r.Header.Get("X-Sgate-Session")
	nonce := r.Header.Get("X-Sgate-Nonce")
	ts := r.Header.Get("X-Sgate-Time")
	resume := r.Header.Get("X-Sgate-Resume")
	credential := r.Header.Get(passwordHeader)
	expected := mac(s.cfg.Key, protocol, "client", id, nonce, ts, resume, r.URL.RequestURI(), credential)
	if r.Method != http.MethodGet || !websocket.IsWebSocketUpgrade(r) || !validID(id) || !validID(nonce) ||
		!freshTimestamp(ts) ||
		(resume != "0" && resume != "1") ||
		!hmac.Equal([]byte(expected), []byte(r.Header.Get("X-Sgate-Auth"))) {
		return tunnelRequest{}, false
	}
	if len(r.Header.Values(passwordHeader)) != 1 ||
		!verifyPassword(s.cfg.Key, s.cfg.Password, credential, id, nonce, ts, resume, r.URL.RequestURI()) {
		return tunnelRequest{}, false
	}
	request := tunnelRequest{id: id, nonce: nonce, timestamp: ts, resume: resume, credential: credential}
	if values, ok := r.URL.Query()[receiveWindowQuery]; ok {
		if len(values) != 1 {
			return tunnelRequest{}, false
		}
		var err error
		request.peerWindow, err = parseReceiveWindow(values[0])
		if err != nil {
			return tunnelRequest{}, false
		}
	}
	if values, ok := r.URL.Query()[streamErrorsQuery]; ok {
		if len(values) != 1 || values[0] != "1" {
			return tunnelRequest{}, false
		}
		request.reportStreamErrors = true
	}
	return request, true
}

// acquireSession 在同一把锁下检查过期、nonce 重放和占用状态。
// 所有返回路径都释放 s.mu；只有确认成功的重连才会在 ServeHTTP 中刷新保留时间。
func (s *Server) acquireSession(w http.ResponseWriter, r *http.Request, request tunnelRequest) (
	*serverEntry,
	time.Time,
	bool,
) {
	id, nonce, ts, resume, credential := request.id, request.nonce, request.timestamp, request.resume, request.credential
	s.mu.Lock()
	s.reapLocked(time.Now())
	if s.closed {
		s.mu.Unlock()
		s.rejectHandshake(w, r, request, "unavailable", http.StatusServiceUnavailable)
		return nil, time.Time{}, false
	}
	if _, exists := s.nonces[nonce]; exists {
		s.mu.Unlock()
		s.cfg.Options.Logf("tunnel handshake rejected: peer=%q session=%s reason=replayed nonce", r.RemoteAddr, id)
		http.NotFound(w, r)
		return nil, time.Time{}, false
	}
	if len(s.nonces) >= 100000 {
		s.mu.Unlock()
		s.rejectHandshake(w, r, request, "nonce cache limit reached", http.StatusServiceUnavailable)
		return nil, time.Time{}, false
	}
	s.nonces[nonce] = time.Now()
	e := s.sessions[id]
	if e != nil {
		e.session.mu.Lock()
		closed := e.session.closed
		e.session.mu.Unlock()
		if closed && !e.inUse {
			delete(s.sessions, id)
			e = nil
		}
	}
	if e == nil {
		if resume == "1" {
			s.mu.Unlock()
			serverNonce := randomID()
			w.Header().Set("X-Sgate-Nonce", serverNonce)
			w.Header().Set("X-Sgate-Auth", mac(s.cfg.Key, protocol, "gone", id, nonce, serverNonce, ts, credential))
			s.rejectHandshake(w, r, request, "session expired", http.StatusGone)
			return nil, time.Time{}, false
		}
		if len(s.sessions) >= s.cfg.MaxSessions {
			s.mu.Unlock()
			s.rejectHandshake(w, r, request, "session limit reached", http.StatusServiceUnavailable)
			return nil, time.Time{}, false
		}
		opts := s.cfg.Options
		opts.reportStreamErrors = request.reportStreamErrors
		logf, target := opts.Logf, request.target
		opts.Logf = func(format string, args ...any) {
			logf("session=%s target=%q "+format, append([]any{id, target}, args...)...)
		}
		e = &serverEntry{session: newSession(s.ctx, request.target, opts), idle: time.Now()}
		s.sessions[id] = e
	}
	// 会话内所有流始终属于首次选择的后端；重连不能把旧缓存送给另一个服务。
	if e.session.target != request.target {
		s.mu.Unlock()
		s.rejectHandshake(w, r, request, "session target mismatch", http.StatusConflict)
		return nil, time.Time{}, false
	}
	if e.session.opts.reportStreamErrors != request.reportStreamErrors {
		s.mu.Unlock()
		s.rejectHandshake(w, r, request, "session diagnostic capability mismatch", http.StatusConflict)
		return nil, time.Time{}, false
	}
	if e.inUse {
		s.mu.Unlock()
		s.rejectHandshake(w, r, request, "session busy", http.StatusConflict)
		return nil, time.Time{}, false
	}
	e.inUse = true
	var resumeDeadline time.Time
	if resume == "1" {
		resumeDeadline = e.idle.Add(s.cfg.SessionTTL)
	}
	s.mu.Unlock()
	return e, resumeDeadline, true
}
