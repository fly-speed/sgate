package tunnel

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"syscall"
)

const streamErrorsQuery = "sgate-stream-errors"

// Only negotiated peers receive a one-byte reason in reliable RESET frames.
// Keep raw dial errors local: they may contain infrastructure details, and must
// never become application payload or an unbounded peer-controlled log message.
const (
	resetDialFailed byte = iota + 1
	resetDialTimeout
	resetDNSFailed
	resetRefused
	resetUnreachable
	resetStreamLimit
	resetReadFailed
	resetWriteFailed
)

type streamReset struct {
	id     uint64
	reason byte
}

func validResetReason(data []byte) bool {
	return len(data) == 0 || len(data) == 1 && data[0] >= resetDialFailed && data[0] <= resetWriteFailed
}

func resetReasonText(data []byte) string {
	if len(data) == 0 {
		return "remote closed the stream (peer supplied no detailed reason)"
	}
	switch data[0] {
	case resetDialFailed:
		return "backend TCP connection failed (see server log)"
	case resetDialTimeout:
		return "backend TCP connection timed out"
	case resetDNSFailed:
		return "backend DNS resolution failed"
	case resetRefused:
		return "backend TCP connection refused"
	case resetUnreachable:
		return "backend network/host unreachable"
	case resetStreamLimit:
		return "server stream limit reached"
	case resetReadFailed:
		return "remote TCP read failed (see remote log)"
	case resetWriteFailed:
		return "remote TCP write/half-close failed (see remote log)"
	default:
		return "invalid remote reset reason"
	}
}

func dialResetReason(err error) byte {
	var dns *net.DNSError
	var network net.Error
	switch {
	case errors.As(err, &dns):
		return resetDNSFailed
	case errors.Is(err, syscall.ECONNREFUSED):
		return resetRefused
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH):
		return resetUnreachable
	case errors.As(err, &network) && network.Timeout():
		return resetDialTimeout
	default:
		return resetDialFailed
	}
}

func (s *session) failStream(id uint64, operation string, err error, reason byte) {
	// The reader and writer can fail together. Log and reset only once, and do
	// not report normal session shutdown as a new application failure.
	if s.ctx.Err() != nil || !s.drop(id) {
		return
	}
	s.opts.Logf("stream=%d target=%q %s failed: %v", id, s.target, operation, err)
	s.enqueueResetReason(id, reason)
}

func (s *Server) rejectHandshake(w http.ResponseWriter, r *http.Request, request tunnelRequest, reason string, status int) {
	s.cfg.Options.Logf("tunnel handshake rejected: peer=%q session=%s target=%q reason=%s HTTP=%d",
		r.RemoteAddr, request.id, request.target, reason, status)
	http.Error(w, reason, status)
}

func mappingLogger(logf func(string, ...any), listen, endpoint string) func(string, ...any) {
	u, _ := url.Parse(endpoint) // NewClient already validated the URL.
	target := u.Query().Get("addr")
	if target == "" {
		target = "(server default)"
	}
	// Never log userinfo or arbitrary query parameters from a configured URL.
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	return func(format string, args ...any) {
		logf("listen=%q server=%q target=%q "+format, append([]any{listen, u.String(), target}, args...)...)
	}
}

func safeDialError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err // url.Error.Error includes the complete request URL.
	}
	return err
}

func handshakeStatusReason(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid tunnel request or backend addr"
	case http.StatusForbidden:
		return "backend target is not allowed"
	case http.StatusNotFound:
		return "tunnel path or authentication rejected"
	case http.StatusConflict:
		return "session busy or session parameters changed"
	case http.StatusGone:
		return "server session expired"
	case http.StatusServiceUnavailable:
		return "server unavailable or capacity limit reached"
	default:
		return http.StatusText(status)
	}
}
