package tunnel

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// 可选后端采用完整 host:port 精确匹配，不按 DNS 解析结果放宽允许列表。
func validateSelectableTarget(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil || host == "" || strings.TrimSpace(address) != address {
		return errors.New("allowed target must be host:port")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 || strings.Trim(port, "0123456789") != "" {
		return errors.New("allowed target requires a numeric port between 1 and 65535")
	}
	if net.ParseIP(host) != nil {
		return nil
	}
	// 域名只允许 DNS 主机名字符，防止 URL、路径或控制字符混入拨号地址。
	name := strings.TrimSuffix(host, ".")
	if len(name) > 253 {
		return errors.New("allowed target hostname is too long")
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return errors.New("invalid allowed target hostname")
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return errors.New("invalid allowed target hostname")
			}
		}
	}
	return nil
}

// UpdateAllowedTargets replaces admission policy atomically. Existing sessions
// keep their bound backend; new requests, including reconnects, use this list.
func (s *Server) UpdateAllowedTargets(targets []string) error {
	allowed := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		if err := validateSelectableTarget(target); err != nil {
			return err
		}
		allowed[target] = struct{}{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("server is closed")
	}
	s.allowedTargets = allowed
	return nil
}

func (s *Server) selectTarget(w http.ResponseWriter, r *http.Request) (string, bool) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		s.cfg.Options.Logf("tunnel target rejected: session=%s peer=%q reason=invalid query", r.Header.Get("X-Sgate-Session"), r.RemoteAddr)
		http.Error(w, "invalid query", http.StatusBadRequest)
		return "", false
	}
	addresses, present := query["addr"]
	if !present {
		return s.cfg.Target, true
	}
	if len(addresses) != 1 || validateSelectableTarget(addresses[0]) != nil {
		s.cfg.Options.Logf("tunnel target rejected: session=%s peer=%q reason=invalid addr (expected host:port)", r.Header.Get("X-Sgate-Session"), r.RemoteAddr)
		http.Error(w, "invalid addr", http.StatusBadRequest)
		return "", false
	}
	s.mu.Lock()
	_, allowed := s.allowedTargets[addresses[0]]
	s.mu.Unlock()
	if !allowed {
		s.cfg.Options.Logf("tunnel target rejected: session=%s peer=%q target=%q reason=target not in allowed-targets", r.Header.Get("X-Sgate-Session"), r.RemoteAddr, addresses[0])
		http.Error(w, "target not allowed", http.StatusForbidden)
		return "", false
	}
	return addresses[0], true
}
