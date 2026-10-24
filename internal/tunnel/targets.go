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

func (s *Server) selectTarget(w http.ResponseWriter, r *http.Request) (string, bool) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		http.Error(w, "invalid query", http.StatusBadRequest)
		return "", false
	}
	addresses, present := query["addr"]
	if !present {
		return s.cfg.Target, true
	}
	if len(addresses) != 1 || validateSelectableTarget(addresses[0]) != nil {
		http.Error(w, "invalid addr", http.StatusBadRequest)
		return "", false
	}
	if _, allowed := s.allowedTargets[addresses[0]]; !allowed {
		http.Error(w, "target not allowed", http.StatusForbidden)
		return "", false
	}
	return addresses[0], true
}
