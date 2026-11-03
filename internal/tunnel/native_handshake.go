package tunnel

import (
	"context"
	"crypto/hmac"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/quic-go/quic-go"
)

const nativeProtocol = "sgate-quic-v2"
const nativeQuery = "sgate-native"
const maxNativeHandshake = 16 * 1024

type nativeHello struct {
	URI     string      `json:"uri"`
	Headers http.Header `json:"headers"`
}
type nativeReply struct {
	Status  int          `json:"status"`
	Headers http.Header  `json:"headers"`
	Probe   *ProbeResult `json:"probe,omitempty"`
}
type nativeResponse struct {
	header http.Header
	status int
}

func (r *nativeResponse) Header() http.Header         { return r.header }
func (r *nativeResponse) WriteHeader(status int)      { r.status = status }
func (r *nativeResponse) Write(p []byte) (int, error) { return len(p), nil }

func writeNativeRecord(w io.Writer, p []byte) error {
	record := make([]byte, 4+len(p))
	binary.BigEndian.PutUint32(record, uint32(len(p)))
	copy(record[4:], p)
	for len(record) > 0 {
		n, err := w.Write(record)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrNoProgress
		}
		record = record[n:]
	}
	return nil
}
func readNativeRecord(r io.Reader, limit int) ([]byte, error) {
	var h [4]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(h[:])
	if n == 0 || uint64(n) > uint64(limit) {
		return nil, errors.New("native record exceeds limit")
	}
	p := make([]byte, int(n))
	_, err := io.ReadFull(r, p)
	return p, err
}
func writeNativeJSON(w io.Writer, v any) error {
	p, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(p) > maxNativeHandshake {
		return errors.New("native handshake exceeds limit")
	}
	return writeNativeRecord(w, p)
}
func readNativeJSON(r io.Reader, v any) error {
	p, err := readNativeRecord(r, maxNativeHandshake)
	if err != nil {
		return err
	}
	return json.Unmarshal(p, v)
}

// NativeQUICServer resolves the current proxy at admission, including shared
// worker replacement. HTTP remains a separate listener and protocol.
func (s *Server) NativeQUICServer() *Server { return s }
func (s *Server) serveNative(conn *quic.Conn) {
	traffic := newNativeTraffic(s.cfg.Options.Stats, conn.RemoteAddr())
	defer traffic.close()
	defer conn.CloseWithError(0, "native transport ended")
	ctx, cancel := context.WithTimeout(conn.Context(), s.cfg.Options.DialTimeout)
	defer cancel()
	control, err := conn.AcceptStream(ctx)
	if err != nil {
		return
	}
	_ = control.SetDeadline(time.Now().Add(s.cfg.Options.DialTimeout))
	handshake := nativeIO{ReadWriter: control, traffic: traffic}
	var hello nativeHello
	if readNativeJSON(handshake, &hello) != nil {
		return
	}
	u, err := url.ParseRequestURI(hello.URI)
	if err != nil || u.IsAbs() || u.Host != "" || u.Path != s.cfg.Path || (len(u.Query()[nativeQuery]) != 1 || u.Query().Get(nativeQuery) != "2") {
		return
	}
	r := &http.Request{Method: http.MethodGet, URL: u, Header: hello.Headers, RemoteAddr: conn.RemoteAddr().String()}
	// Only the representation is reused; no HTTP request or Upgrade is sent.
	if r.Header == nil {
		return
	}
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "websocket")
	request, ok := s.authenticateRequest(r)
	if !ok {
		return
	}
	request.native = true
	request.nativeWindow = max(frameHeader+chunkSize, request.peerWindow/8)
	if values, present := u.Query()["sgate-flow-window"]; present {
		if len(values) != 1 {
			return
		}
		n, err := parseReceiveWindow(values[0])
		if err != nil || n > request.peerWindow {
			return
		}
		request.nativeWindow = n
	}
	response := &nativeResponse{header: make(http.Header), status: http.StatusOK}
	request.target, ok = s.selectTarget(response, r)
	if !ok {
		_ = writeNativeJSON(handshake, nativeReply{Status: response.status, Headers: response.header})
		_ = control.Close()
		waitNativePeer(conn, s.cfg.Options.WriteTimeout)
		return
	}
	serverNonce := randomID()
	if probes, present := u.Query()[probeQuery]; present {
		if len(probes) != 1 || probes[0] != "1" {
			return
		}
		s.serveNativeProbe(conn, control, traffic, r, request, serverNonce)
		return
	}
	entry, deadline, ok := s.acquireSession(response, r, request)
	if !ok {
		_ = writeNativeJSON(handshake, nativeReply{Status: response.status, Headers: response.header})
		_ = control.Close()
		waitNativePeer(conn, s.cfg.Options.WriteTimeout)
		return
	}
	confirmed := false
	defer func() {
		s.mu.Lock()
		entry.inUse = false
		entry.ready = false
		if confirmed {
			entry.idle = time.Now()
		}
		s.mu.Unlock()
	}()
	if err := writeNativeJSON(handshake, nativeReply{Status: http.StatusOK, Headers: s.handshakeHeaders(request, serverNonce)}); err != nil {
		return
	}
	nt := newNativeTransport(conn, control, newCodec(s.cfg.Key, request.id, request.nonce, serverNonce, false), s.cfg.Key, request.id, request.nonce, serverNonce)
	nt.traffic = traffic
	nt.peerLimit = request.nativeWindow
	err = entry.session.runNative(nt, deadline, func() { confirmed = true; s.mu.Lock(); entry.ready = true; s.mu.Unlock() })
	if errors.Is(err, errPeerSessionClosed) {
		s.mu.Lock()
		if s.sessions[request.id] == entry {
			delete(s.sessions, request.id)
		}
		s.mu.Unlock()
	}
}
func waitNativePeer(conn *quic.Conn, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-conn.Context().Done():
	case <-timer.C:
	}
}

func (s *Server) serveNativeProbe(conn *quic.Conn, control *quic.Stream, traffic *nativeTraffic, r *http.Request, request tunnelRequest, nonce string) {
	select {
	case s.probeGate <- struct{}{}:
		defer func() { <-s.probeGate }()
	default:
		return
	}
	s.mu.Lock()
	s.reapLocked(time.Now())
	_, replayed := s.nonces[request.nonce]
	allowed := !s.closed && s.authorizedLocked(request) && !replayed && len(s.nonces) < 100000
	if allowed {
		s.nonces[request.nonce] = time.Now()
	}
	s.mu.Unlock()
	if !allowed {
		return
	}
	result := &ProbeResult{TestID: request.id, Stage: "backend", Target: request.target}
	ctx, cancel := context.WithTimeout(s.ctx, s.cfg.Options.DialTimeout)
	defer cancel()
	backend, err := (&net.Dialer{}).DialContext(ctx, "tcp", request.target)
	if err != nil {
		result.Error = probeDiagnostic(err.Error())
		result.Detail = resetReasonText([]byte{dialResetReason(err)})
	} else {
		backend.Close()
		result.OK = true
		result.Detail = "Tunnel authenticated; backend TCP reachable"
	}
	// The MAC covers the result as well as the normal server proof.
	headers := s.handshakeHeaders(request, nonce)
	p, _ := json.Marshal(result)
	headers.Set("X-Sgate-Native-Probe-Auth", mac(s.cfg.Key, nativeProtocol, request.id, request.nonce, nonce, string(p)))
	_ = writeNativeJSON(nativeIO{ReadWriter: control, traffic: traffic}, nativeReply{Status: http.StatusOK, Headers: headers, Probe: result})
	_ = control.Close()
	waitNativePeer(conn, s.cfg.Options.WriteTimeout)
}
func (c *Client) nativeQUIC() bool {
	u, _ := url.Parse(c.cfg.URL)
	return u.Scheme == "quic" && c.cfg.QUICMode != "legacy"
}
func (c *Client) dialNative(ctx context.Context, id string, resume bool) (*nativeTransport, *ProbeResult, int, error) {
	u, _ := url.Parse(c.cfg.URL)
	q := u.Query()
	q.Set(nativeQuery, "2")
	q.Del(reverseQuery)
	q.Del(reverseTLSQuery)
	if c.cfg.AllowReverse && c.cfg.Username != "" {
		q.Set(reverseQuery, "1")
		q.Set(reverseTLSQuery, "1")
	}
	q.Set(streamErrorsQuery, "1")
	q.Set(receiveWindowQuery, strconv.Itoa(c.cfg.Options.ReceiveBytes))
	q.Set("sgate-flow-window", strconv.Itoa(c.cfg.Options.NativeStreamBuffer))
	u.RawQuery = q.Encode()
	nonce, ts := randomID(), strconv.FormatInt(time.Now().Unix(), 10)
	resumeText := "0"
	if resume {
		resumeText = "1"
	}
	headers, err := accountRequestHeaders(c.cfg.Key, c.cfg.Username, c.cfg.Password, id, nonce, ts, resumeText, u.RequestURI())
	if err != nil {
		return nil, nil, 0, err
	}
	config := quicTLS(c.cfg.TLSConfig)
	config.NextProtos = []string{nativeProtocol}
	if config.ServerName == "" {
		config.ServerName = u.Hostname()
	}
	conn, err := dialQUICConnection(ctx, u.Host, config, c.cfg.Options)
	if err != nil {
		return nil, nil, 0, safeDialError(err)
	}
	traffic := newNativeTraffic(c.cfg.Options.Stats, conn.RemoteAddr())
	success := false
	defer func() {
		if !success {
			traffic.close()
			conn.CloseWithError(0, "handshake failed")
		}
	}()
	control, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, nil, 0, err
	}
	stop := context.AfterFunc(ctx, func() { conn.CloseWithError(0, "handshake canceled") })
	defer stop()
	deadline := time.Now().Add(c.cfg.Options.DialTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = control.SetDeadline(deadline)
	handshake := nativeIO{ReadWriter: control, traffic: traffic}
	if err := writeNativeJSON(handshake, nativeHello{URI: u.RequestURI(), Headers: headers}); err != nil {
		return nil, nil, 0, err
	}
	var reply nativeReply
	if err := readNativeJSON(handshake, &reply); err != nil {
		return nil, nil, 0, err
	}
	serverNonce := reply.Headers.Get("X-Sgate-Nonce")
	if reply.Status == http.StatusGone {
		expected := mac(c.cfg.Key, protocol, "gone", id, nonce, serverNonce, ts, headers.Get(passwordHeader))
		if !validID(serverNonce) || !hmac.Equal([]byte(expected), []byte(reply.Headers.Get("X-Sgate-Auth"))) {
			return nil, nil, 0, errors.New("unauthenticated native session expiry")
		}
	}
	if reply.Status != http.StatusOK {
		return nil, nil, reply.Status, fmt.Errorf("native handshake status %d", reply.Status)
	}
	window := reply.Headers.Get(receiveWindowHeader)
	expected := windowMAC(c.cfg.Key, window, protocol, "server", id, nonce, serverNonce, ts, headers.Get(passwordHeader))
	if !validID(serverNonce) || !hmac.Equal([]byte(expected), []byte(reply.Headers.Get("X-Sgate-Auth"))) {
		return nil, nil, 0, errors.New("native server authentication failed")
	}
	peerWindow, err := parseReceiveWindow(window)
	if err != nil {
		return nil, nil, 0, err
	}
	if q.Get(probeQuery) == "1" {
		if reply.Probe == nil || reply.Probe.TestID != id {
			return nil, nil, 0, errors.New("invalid native probe response")
		}
		p, _ := json.Marshal(reply.Probe)
		proof := mac(c.cfg.Key, nativeProtocol, id, nonce, serverNonce, string(p))
		if !hmac.Equal([]byte(proof), []byte(reply.Headers.Get("X-Sgate-Native-Probe-Auth"))) {
			return nil, nil, 0, errors.New("native probe authentication failed")
		}
		return nil, reply.Probe, http.StatusOK, nil
	}
	if reply.Probe != nil {
		return nil, nil, 0, errors.New("unexpected native probe response")
	}
	crypt := newCodec(c.cfg.Key, id, nonce, serverNonce, true)
	crypt.reportStreamErrors = true
	nt := newNativeTransport(conn, control, crypt, c.cfg.Key, id, nonce, serverNonce)
	nt.traffic = traffic
	nt.peerLimit = max(frameHeader+chunkSize, peerWindow/8)
	if flowWindow := reply.Headers.Get("X-Sgate-Flow-Window"); flowWindow != "" {
		proof := mac(c.cfg.Key, nativeProtocol, "flow-window", id, nonce, serverNonce, flowWindow)
		n, err := parseReceiveWindow(flowWindow)
		if err != nil || n > peerWindow || !hmac.Equal([]byte(proof), []byte(reply.Headers.Get("X-Sgate-Flow-Window-Auth"))) {
			return nil, nil, 0, errors.New("invalid native flow window proof")
		}
		nt.peerLimit = n
	}
	_ = control.SetDeadline(time.Time{})
	success = true
	return nt, nil, http.StatusOK, nil
}
