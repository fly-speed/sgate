package admin

import (
	"bytes"
	"crypto"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"sgate/internal/configfile"
	"sgate/internal/tunnel"
)

const maxVPNServerFile = 256 << 10

type vpnServerFile struct {
	Path     string `json:"path"`
	Revision string `json:"revision,omitempty"`
}

func (h *Handler) vpnServerFiles(w http.ResponseWriter, r *http.Request) {
	if h.view.Role != "server" || h.view.ServerVPNFilePaths == nil {
		problem(w, 404, "服务端 VPN 文件管理不可用")
		return
	}
	var input struct {
		Field    string `json:"field"`
		Revision string `json:"revision"`
		Content  string `json:"content"`
		Create   bool   `json:"create"`
	}
	if r.Method == http.MethodPut && !decode(w, r, &input, 2*maxVPNServerFile+1024) {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, err := h.readSession(r); err != nil {
		problem(w, 401, "登录已过期，请重新登录")
		return
	}
	paths, err := h.view.ServerVPNFilePaths()
	if err != nil {
		problem(w, 400, "无法读取服务端 VPN 文件配置")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodGet {
		files := make(map[string]vpnServerFile, len(paths))
		for field, path := range paths {
			revision := vpnServerFileRevision(path)
			if field == "vpn-tls-cert" || field == "vpn-tls-key" {
				if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
					path = ""
				}
			}
			files[field] = vpnServerFile{Path: path, Revision: revision}
		}
		reply(w, 200, files)
		return
	}
	path := paths[input.Field]
	current := vpnServerFileRevision(path)
	creating := input.Create && (input.Field == "vpn-tls-cert" || input.Field == "vpn-tls-key")
	if (creating && current != "") || (!creating && current == "") || current != input.Revision {
		problem(w, 409, "VPN 文件或路径已变化，或文件不可覆盖，请重新打开向导")
		return
	}
	data, message := validateVPNServerFile(input.Field, input.Content)
	if message != "" {
		problem(w, 400, message)
		return
	}
	defer clear(data)
	if creating {
		path, err = h.createVPNServerFile(input.Field, data)
	} else {
		err = atomicFile(path, data)
	}
	if err != nil {
		problem(w, 500, "无法覆盖服务端 VPN 文件")
		return
	}
	h.credentialFilesPending = true
	reply(w, 200, vpnServerFile{Path: path, Revision: digest([]byte(path + ":" + digest(data)))})
}

// Imports with no readable configured file get a new private path. The wizard
// saves that path with the rest of its draft when the user applies it.
func (h *Handler) createVPNServerFile(field string, data []byte) (string, error) {
	dir, err := filepath.Abs(filepath.Dir(h.view.AppConfigPath))
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, field+"-*.pem")
	if err != nil {
		return "", err
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
		return "", err
	}
	return f.Name(), nil
}

func vpnServerFileRevision(path string) string {
	raw, err := regularRead(path, maxVPNServerFile)
	if err != nil {
		return ""
	}
	defer clear(raw)
	return digest([]byte(path + ":" + digest(raw)))
}

func validateVPNServerFile(field, content string) ([]byte, string) {
	if len(content) > maxVPNServerFile {
		return nil, "文件过大"
	}
	switch field {
	case "vpn-key-file":
		key, err := tunnel.ParseKey(strings.TrimSpace(content))
		if err != nil {
			return nil, "共享密钥必须是 64 个十六进制字符"
		}
		defer clear(key)
		return append(hex.AppendEncode(nil, key), '\n'), ""
	case "vpn-tls-cert":
		data := []byte(content)
		chain := bytes.TrimSpace(data)
		if len(chain) == 0 {
			return nil, "请上传有效的 PEM 证书"
		}
		for len(chain) != 0 {
			block, rest := pem.Decode(chain)
			if !bytes.HasPrefix(chain, []byte("-----BEGIN CERTIFICATE-----")) || block == nil || block.Type != "CERTIFICATE" {
				return nil, "请上传有效的 PEM 证书"
			}
			if _, err := x509.ParseCertificate(block.Bytes); err != nil {
				return nil, "请上传有效的 PEM 证书"
			}
			chain = bytes.TrimSpace(rest)
		}
	case "vpn-tls-key":
		raw := []byte(content)
		defer clear(raw)
		block, rest := pem.Decode(raw)
		if block == nil || len(bytes.TrimSpace(rest)) != 0 {
			return nil, "请上传有效的未加密 PEM 私钥"
		}
		defer clear(block.Bytes)
		switch block.Type {
		case "PRIVATE KEY":
			if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
				if _, ok := key.(crypto.Signer); ok {
					return []byte(content), ""
				}
			}
		case "RSA PRIVATE KEY":
			if _, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
				return []byte(content), ""
			}
		case "EC PRIVATE KEY":
			if _, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
				return []byte(content), ""
			}
		}
		return nil, "请上传有效的未加密 PEM 私钥"
	default:
		return nil, "无效的 VPN 文件类型"
	}
	return []byte(content), ""
}
