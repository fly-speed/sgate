package admin

import (
	"crypto/x509"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"sgate/internal/configfile"
	"sgate/internal/tunnel"
)

const maxVPNCAFile = 256 << 10

func (h *Handler) importVPNClientFile(w http.ResponseWriter, r *http.Request) {
	if h.view.Role != "client" {
		problem(w, 404, "仅客户端支持导入 VPN 文件")
		return
	}
	var input struct {
		Kind    string `json:"kind"`
		Content string `json:"content"`
	}
	if !decode(w, r, &input, 2*maxVPNCAFile+1024) {
		return
	}
	var data []byte
	var pattern string
	switch input.Kind {
	case "key":
		key, err := tunnel.ParseKey(strings.TrimSpace(input.Content))
		if err != nil {
			problem(w, 400, "共享密钥必须是 64 个十六进制字符")
			return
		}
		defer clear(key)
		data = append(hex.AppendEncode(nil, key), '\n')
		defer clear(data)
		pattern = "vpn-client-*.key"
	case "ca":
		data = []byte(input.Content)
		if len(data) > maxVPNCAFile || !x509.NewCertPool().AppendCertsFromPEM(data) {
			problem(w, 400, "请上传不超过 256 KiB 的 PEM 证书文件")
			return
		}
		pattern = "vpn-ca-*.pem"
	default:
		problem(w, 400, "无效的 VPN 文件类型")
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if _, err := h.readSession(r); err != nil {
		problem(w, 401, "登录已过期，请重新登录")
		return
	}
	// Use a new path so importing a draft cannot replace a running VPN or proxy key.
	dir, err := filepath.Abs(filepath.Dir(h.view.AppConfigPath))
	if err != nil {
		problem(w, 500, "无法保存 VPN 文件")
		return
	}
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		problem(w, 500, "无法保存 VPN 文件")
		return
	}
	if err = configfile.ProtectFile(f); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = configfile.SyncDirectory(dir)
	}
	if err != nil {
		os.Remove(f.Name())
		problem(w, 500, "无法保存 VPN 文件")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	reply(w, 200, map[string]string{"path": f.Name()})
}
