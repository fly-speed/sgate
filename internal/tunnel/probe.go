package tunnel

import (
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
)

const (
	probeQuery           = "sgate-probe"
	probeCommandProtocol = "command-v1"
	probeConnectCommand  = "TEST_CONNECT"
	probeResultCommand   = "TEST_CONNECT_RESULT"
	maxProbeMessageBytes = 1024
)

// ProbeResult describes a short-lived authenticated test, not application health.
type ProbeResult struct {
	TestID    string `json:"test_id"`
	OK        bool   `json:"ok"`
	Stage     string `json:"stage"`
	Detail    string `json:"detail"`
	ElapsedMS int64  `json:"elapsed_ms"`
}

// These messages travel inside the authenticated AEAD WebSocket channel, never
// through a business stream. The HTTP query only selects the short-lived mode.
type probeCommand struct {
	Command string `json:"command"`
	TestID  string `json:"test_id"`
}

type probeReply struct {
	Status  string `json:"status"`
	Command string `json:"command"`
	TestID  string `json:"test_id"`
	Reason  byte   `json:"reason"` // 0 = reachable; other values reuse bounded dial failure codes.
}

// Probe performs the explicit encrypted TEST_CONNECT / TEST_CONNECT_RESULT
// exchange. It does not bind local ports, create sessions or send app bytes.
func (c *Client) Probe(ctx context.Context) (result ProbeResult) {
	started, id := time.Now(), randomID()
	result = ProbeResult{TestID: id, Stage: "tunnel"}
	logf := mappingLogger(c.cfg.Options.Logf, "(probe)", c.cfg.URL)
	logf("test_id=%s command=%s started", id, probeConnectCommand)
	defer func() {
		result.ElapsedMS = time.Since(started).Milliseconds()
		logf("test_id=%s command=%s completed: ok=%t stage=%s reason=%q elapsed_ms=%d",
			id, probeConnectCommand, result.OK, result.Stage, result.Detail, result.ElapsedMS)
	}()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	probing := *c
	u, _ := url.Parse(probing.cfg.URL)
	query := u.Query()
	query.Set(probeQuery, "1")
	u.RawQuery = query.Encode()
	probing.cfg.URL, probing.cfg.Options.Stats = u.String(), nil
	// Legacy servers reject this unknown resumed ID without retaining a session.
	ws, crypt, status, err := probing.dial(ctx, id, true)
	if err != nil {
		result.Detail = err.Error()
		switch status {
		case http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable:
			result.Detail = handshakeStatusReason(status)
		}
		return result
	}
	defer ws.Close()
	stop := prepareProbeConn(ctx, ws)
	defer stop()
	result.Stage = "command"
	reply, err := exchangeProbeCommand(ws, crypt, id, logf)
	if err != nil {
		result.Detail = err.Error()
		return result
	}
	result.Stage, result.OK = "backend", reply.Reason == 0
	if result.OK {
		result.Detail = "Tunnel authenticated; backend TCP reachable"
	} else {
		result.Detail = resetReasonText([]byte{reply.Reason})
	}
	return result
}

func exchangeProbeCommand(ws *websocket.Conn, crypt *codec, id string, logf func(string, ...any)) (probeReply, error) {
	if err := writeProbeMessage(ws, crypt, probeCommand{Command: probeConnectCommand, TestID: id}); err != nil {
		return probeReply{}, err
	}
	logf("test_id=%s command=%s sent", id, probeConnectCommand)
	var reply probeReply
	if err := readProbeMessage(ws, crypt, &reply); err != nil {
		return probeReply{}, err
	}
	if reply.Command != probeResultCommand || reply.TestID != id || reply.Reason > resetUnreachable ||
		(reply.Status != "ok" && reply.Status != "failed") || (reply.Status == "ok") != (reply.Reason == 0) {
		return probeReply{}, errors.New("invalid connectivity test result command or test ID")
	}
	logf("test_id=%s command=%s received: status=%s reason=%d", id, probeResultCommand, reply.Status, reply.Reason)
	return reply, nil
}

// Context cancellation must interrupt reads, including silent or slow fragments.
// Close the underlying TCP socket directly so TLS writers cannot delay cleanup.
func prepareProbeConn(ctx context.Context, ws *websocket.Conn) func() bool {
	ws.SetReadLimit(maxProbeMessageBytes)
	if deadline, ok := ctx.Deadline(); ok {
		_ = ws.SetReadDeadline(deadline)
		_ = ws.SetWriteDeadline(deadline)
	}
	return context.AfterFunc(ctx, func() {
		conn := ws.NetConn()
		if wrapped, ok := conn.(interface{ NetConn() net.Conn }); ok {
			conn = wrapped.NetConn()
		}
		_ = conn.SetDeadline(time.Now())
		_ = conn.Close()
	})
}

func writeProbeMessage(ws *websocket.Conn, crypt *codec, value any) error {
	plain, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return ws.WriteMessage(websocket.BinaryMessage, crypt.seal(plain))
}

func readProbeMessage(ws *websocket.Conn, crypt *codec, value any) error {
	kind, sealed, err := ws.ReadMessage()
	if err != nil {
		return err
	}
	if kind != websocket.BinaryMessage {
		return errors.New("connectivity test requires an encrypted binary command")
	}
	plain, err := crypt.open(sealed)
	if err != nil {
		return errors.New("connectivity test command authentication failed")
	}
	decoder := json.NewDecoder(bytes.NewReader(plain))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return errors.New("invalid connectivity test command")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("invalid connectivity test command")
	}
	return nil
}

func probeMAC(key []byte, request tunnelRequest, serverNonce, result string) string {
	return mac(key, protocol, "probe", request.id, request.nonce, serverNonce,
		request.timestamp, request.credential, result)
}

func verifyProbeResponse(key []byte, response *http.Response, request tunnelRequest) (string, bool) {
	result := response.Header.Get("X-Sgate-Probe")
	nonce := response.Header.Get("X-Sgate-Nonce")
	expected := probeMAC(key, request, nonce, result)
	return result, validID(nonce) && hmac.Equal([]byte(expected), []byte(response.Header.Get("X-Sgate-Probe-Auth")))
}

func probeResponseError(key []byte, response *http.Response, request tunnelRequest) error {
	switch response.StatusCode {
	case http.StatusGone:
		return errors.New("server does not support connectivity tests; upgrade the server")
	case http.StatusBadGateway:
		_, verified := verifyProbeResponse(key, response, request)
		if !verified {
			return errors.New("invalid connectivity test response")
		}
		return errors.New("server does not support connectivity tests; upgrade the server")
	default:
		return nil
	}
}

func (s *Server) handshakeHeaders(request tunnelRequest, serverNonce string) http.Header {
	headers := http.Header{}
	headers.Set("X-Sgate-Nonce", serverNonce)
	window := ""
	if request.peerWindow > 0 {
		window = strconv.Itoa(s.cfg.Options.BufferBytes)
		headers.Set(receiveWindowHeader, window)
	}
	headers.Set("X-Sgate-Auth", windowMAC(s.cfg.Key, window, protocol, "server", request.id,
		request.nonce, serverNonce, request.timestamp, request.credential))
	return headers
}

func (s *Server) serveProbe(w http.ResponseWriter, r *http.Request, request tunnelRequest) {
	select {
	case s.probeGate <- struct{}{}:
		defer func() { <-s.probeGate }()
	default:
		s.rejectHandshake(w, r, request, "connectivity tests busy", http.StatusServiceUnavailable)
		return
	}
	s.mu.Lock()
	s.reapLocked(time.Now())
	_, replay := s.nonces[request.nonce]
	allowed := !s.closed && !replay && len(s.nonces) < 100000
	if allowed {
		s.nonces[request.nonce] = time.Now()
	}
	s.mu.Unlock()
	if !allowed {
		s.rejectHandshake(w, r, request, "connectivity test unavailable or replayed", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), min(s.cfg.Options.DialTimeout, 5*time.Second))
	defer cancel()
	stopServer := context.AfterFunc(s.ctx, cancel)
	defer stopServer()
	nonce := randomID()
	headers := s.handshakeHeaders(request, nonce)
	headers.Set("X-Sgate-Probe", probeCommandProtocol)
	headers.Set("X-Sgate-Probe-Auth", probeMAC(s.cfg.Key, request, nonce, probeCommandProtocol))
	ws, err := (&websocket.Upgrader{HandshakeTimeout: s.cfg.Options.DialTimeout}).Upgrade(w, r, headers)
	if err != nil {
		s.cfg.Options.Logf("test_id=%s command=%s upgrade failed: peer=%q target=%q reason=%v",
			request.id, probeConnectCommand, r.RemoteAddr, request.target, err)
		return
	}
	defer ws.Close()
	stop := prepareProbeConn(ctx, ws)
	defer stop()
	crypt := newCodec(s.cfg.Key, request.id, request.nonce, nonce, false)
	s.executeProbeCommand(ctx, ws, crypt, request, r.RemoteAddr)
}

func (s *Server) executeProbeCommand(ctx context.Context, ws *websocket.Conn, crypt *codec, request tunnelRequest, peer string) {
	var command probeCommand
	if err := readProbeMessage(ws, crypt, &command); err != nil {
		s.cfg.Options.Logf("test_id=%s command=%s rejected: peer=%q reason=%v", request.id, probeConnectCommand, peer, err)
		return
	}
	if command.Command != probeConnectCommand || command.TestID != request.id {
		s.cfg.Options.Logf("test_id=%s command=%s rejected: peer=%q reason=invalid command or test ID", request.id, probeConnectCommand, peer)
		return
	}
	s.cfg.Options.Logf("test_id=%s command=%s received: peer=%q target=%q", request.id, probeConnectCommand, peer, request.target)
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", request.target)
	reply := probeReply{Command: probeResultCommand, TestID: request.id, Status: "ok"}
	if err != nil {
		reply.Status, reply.Reason = "failed", dialResetReason(err)
		s.cfg.Options.Logf("test_id=%s command=%s backend failed: target=%q reason=%v", request.id, probeConnectCommand, request.target, err)
	} else {
		_ = conn.Close()
		s.cfg.Options.Logf("test_id=%s command=%s backend connected: target=%q", request.id, probeConnectCommand, request.target)
	}
	if err := writeProbeMessage(ws, crypt, reply); err != nil {
		s.cfg.Options.Logf("test_id=%s command=%s response failed: peer=%q reason=%v", request.id, probeResultCommand, peer, err)
		return
	}
	s.cfg.Options.Logf("test_id=%s command=%s sent: target=%q ok=%t reason=%d", request.id, probeResultCommand, request.target, reply.Reason == 0, reply.Reason)
}
