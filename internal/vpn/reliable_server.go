package vpn

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

func (s *Server) listenReliable() error {
	raw, err := net.Listen("tcp", s.cfg.TCPListen)
	if err != nil {
		return err
	}
	config := s.cfg.TLS.Clone()
	config.NextProtos = []string{"http/1.1"}
	s.tcp = tls.NewListener(raw, config)
	var admitted atomic.Int64
	s.http = &http.Server{ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 8192, IdleTimeout: 30 * time.Second, ErrorLog: log.New(io.Discard, "", 0), Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handlerMu.Lock()
		if s.handlersClosed {
			s.handlerMu.Unlock()
			http.Error(w, "VPN stopped", 503)
			return
		}
		s.handlerWG.Add(1)
		s.handlerMu.Unlock()
		defer s.handlerWG.Done()
		if r.Method != "GET" || r.URL.Path != "/vpn" || r.URL.RawQuery != "" {
			http.NotFound(w, r)
			return
		}
		if admitted.Add(1) > int64(max(s.cfg.Live.Snapshot().MaxClients, int(s.cfg.Stats.active.Load()))+16) {
			admitted.Add(-1)
			http.Error(w, "VPN connection limit", 503)
			return
		}
		defer admitted.Add(-1)
		var raw net.Conn
		if websocket.IsWebSocketUpgrade(r) {
			found := false
			for _, p := range websocket.Subprotocols(r) {
				if p == Protocol {
					found = true
				}
			}
			if !found {
				http.Error(w, "VPN subprotocol required", 400)
				return
			}
			upgrader := websocket.Upgrader{Subprotocols: []string{Protocol}, HandshakeTimeout: 10 * time.Second, ReadBufferSize: 4096, WriteBufferSize: 4096}
			ws, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			ws.SetReadLimit(8192)
			raw = &websocketConn{Conn: ws}
		} else {
			if !strings.EqualFold(r.Header.Get("Upgrade"), Protocol) || !headerToken(r.Header.Get("Connection"), "upgrade") || r.ContentLength > 0 || len(r.TransferEncoding) > 0 || r.Header.Get("Origin") != "" {
				http.Error(w, "VPN upgrade required", 400)
				return
			}
			conn, buffer, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			conn.SetDeadline(time.Now().Add(10 * time.Second))
			if _, err = buffer.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + Protocol + "\r\n\r\n"); err == nil {
				err = buffer.Flush()
			}
			if err != nil {
				conn.Close()
				return
			}
			conn.SetDeadline(time.Time{})
			raw = &bufferedConnection{Conn: conn, reader: buffer.Reader}
		}
		conn := newRecordConnection(raw)
		s.reliableConns.Store(conn, true)
		defer s.reliableConns.Delete(conn)
		defer conn.CloseWithError(0, "VPN session ended")
		select {
		case s.incoming <- conn:
		case <-s.closed:
			return
		}
		select {
		case <-conn.Context().Done():
		case <-s.closed:
		}
	})}
	return nil
}
func headerToken(value, token string) bool {
	for _, v := range strings.Split(value, ",") {
		if strings.EqualFold(strings.TrimSpace(v), token) {
			return true
		}
	}
	return false
}
func (s *Server) acceptConnections(ctx context.Context) {
	s.acceptUDP(ctx)
	if s.listener != nil {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			for {
				q, err := s.listener.Accept(ctx)
				if err != nil {
					if ctx.Err() == nil {
						select {
						case s.acceptErr <- err:
						default:
						}
					}
					return
				}
				select {
				case s.incoming <- quicConnection{q}:
				case <-ctx.Done():
					q.CloseWithError(0, "VPN stopped")
					return
				}
			}
		}()
	}
	if s.http != nil {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			err := s.http.Serve(s.tcp)
			if err != nil && !errors.Is(err, http.ErrServerClosed) && ctx.Err() == nil {
				select {
				case s.acceptErr <- err:
				default:
				}
			}
		}()
	}
}
