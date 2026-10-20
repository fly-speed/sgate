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
	Key         []byte
	Target      string
	Path        string
	SessionTTL  time.Duration
	MaxSessions int
	Options     Options
}
type serverEntry struct {
	session *session
	inUse   bool
	idle    time.Time
}

type Server struct {
	cfg      ServerConfig
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	sessions map[string]*serverEntry
	nonces   map[string]time.Time
	closed   bool
}

func NewServer(cfg ServerConfig) (*Server, error) {
	if len(cfg.Key) != 32 {
		return nil, errors.New("server key must contain 32 bytes")
	}
	if _, _, err := net.SplitHostPort(cfg.Target); err != nil {
		return nil, errors.New("target must be host:port")
	}
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
	s := &Server{cfg: cfg, ctx: ctx, cancel: cancel, sessions: make(map[string]*serverEntry), nonces: make(map[string]time.Time)}
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
	id := r.Header.Get("X-Sgate-Session")
	nonce := r.Header.Get("X-Sgate-Nonce")
	ts := r.Header.Get("X-Sgate-Time")
	resume := r.Header.Get("X-Sgate-Resume")
	expected := mac(s.cfg.Key, protocol, "client", id, nonce, ts, resume, r.URL.RequestURI())
	if r.Method != http.MethodGet || !websocket.IsWebSocketUpgrade(r) || !validID(id) || !validID(nonce) || !freshTimestamp(ts) || (resume != "0" && resume != "1") || !hmac.Equal([]byte(expected), []byte(r.Header.Get("X-Sgate-Auth"))) {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	s.reapLocked(time.Now())
	if s.closed {
		s.mu.Unlock()
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	if _, exists := s.nonces[nonce]; exists {
		s.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	if len(s.nonces) >= 100000 {
		s.mu.Unlock()
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
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
			w.Header().Set("X-Sgate-Auth", mac(s.cfg.Key, protocol, "gone", id, nonce, serverNonce, ts))
			http.Error(w, "session expired", http.StatusGone)
			return
		}
		if len(s.sessions) >= s.cfg.MaxSessions {
			s.mu.Unlock()
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		e = &serverEntry{session: newSession(s.ctx, s.cfg.Target, s.cfg.Options), idle: time.Now()}
		s.sessions[id] = e
	}
	if e.inUse {
		s.mu.Unlock()
		http.Error(w, "session busy", http.StatusConflict)
		return
	}
	e.inUse = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); e.inUse = false; e.idle = time.Now(); s.mu.Unlock() }()
	serverNonce := randomID()
	headers := http.Header{}
	headers.Set("X-Sgate-Nonce", serverNonce)
	headers.Set("X-Sgate-Auth", mac(s.cfg.Key, protocol, "server", id, nonce, serverNonce, ts))
	upgrader := websocket.Upgrader{HandshakeTimeout: s.cfg.Options.DialTimeout, ReadBufferSize: chunkSize, WriteBufferSize: chunkSize}
	ws, err := upgrader.Upgrade(w, r, headers)
	if err != nil {
		return
	}
	s.cfg.Options.Logf("tunnel connected")
	err = e.session.transport(ws, newCodec(s.cfg.Key, id, nonce, serverNonce, false))
	s.cfg.Options.Logf("tunnel disconnected: %v", err)
}
