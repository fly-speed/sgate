package admin

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"sgate/internal/proxyconfig"
)

// mappingConfig reads the inline values without exposing credentials to the page.
func mappingConfig(content []byte) ([]proxyMapping, string, error) {
	fields, err := configFields(string(content))
	if err != nil {
		return nil, "", err
	}
	raw, path := "", ""
	for _, f := range fields {
		if f.Key == "proxies" {
			raw = f.Value
		}
		if f.Key == "proxies-file" {
			path = f.Value
		}
	}
	if path != "" {
		return nil, path, nil
	}
	mappings, err := proxyMappings(raw)
	return mappings, path, err
}

func (h *Handler) savedMappings(content []byte) ([]proxyMapping, string, error) {
	mappings, path, _, err := h.mappingSnapshot(content)
	return mappings, path, err
}

func (h *Handler) mappingSnapshot(content []byte) ([]proxyMapping, string, string, error) {
	mappings, path, err := mappingConfig(content)
	if path == "" {
		return mappings, "", digest(content), err
	}
	path = proxyconfig.Path(h.view.AppConfigPath, path)
	raw, err := proxyconfig.Read(path)
	if err != nil {
		return nil, path, "", err
	}
	mappings, err = proxyMappings(string(raw))
	return mappings, path, digest([]byte(digest(content) + ":" + digest(raw))), err
}

func maskedMappings(mappings []proxyMapping) []proxyMapping {
	out := append([]proxyMapping{}, mappings...)
	for i := range out {
		if out[i].Password != nil {
			marker := secretMarker
			out[i].Password = &marker
		}
	}
	return out
}

func restoreMappingPasswords(next, previous []proxyMapping) ([]proxyMapping, error) {
	out := append([]proxyMapping{}, next...)
	saved := map[string]*string{}
	for _, m := range previous {
		saved[m.Listen] = m.Password
	}
	for i := range out {
		if out[i].Password != nil && *out[i].Password == secretMarker {
			if saved[out[i].Listen] == nil {
				return nil, errors.New("映射监听地址已改变或没有已保存密码，请重新填写该映射密码")
			}
			password := *saved[out[i].Listen]
			out[i].Password = &password
		}
	}
	return out, nil
}

// Configuration revisions include external mappings, preventing stale pages from
// overwriting passwords or mappings changed by another administrator or editor.
func (h *Handler) configRevision(content []byte) string {
	if h.view.Role != "client" {
		return digest(content)
	}
	_, _, revision, err := h.mappingSnapshot(content)
	if err == nil {
		return revision
	}
	return digest([]byte(digest(content) + ":unreadable-mappings"))
}

type mappingFileSave struct {
	configured         string
	path               string
	original, updated  []byte
	existed, committed bool
	staged             string
}

func (s *mappingFileSave) cleanup() {
	if s != nil && s.staged != "" {
		_ = os.Remove(s.staged)
	}
}
func (s *mappingFileSave) write() error {
	if s == nil {
		return nil
	}
	current, err := regularRead(s.path, proxyconfig.MaxBytes)
	if s.existed {
		if err != nil || digest(current) != digest(s.original) {
			return errors.New("映射文件已被其他操作修改，请重新加载")
		}
		if err = atomicFile(s.path+".bak", s.original); err != nil {
			return errors.New("无法备份映射文件")
		}
		err = atomicFile(s.path, s.updated)
	} else {
		if !os.IsNotExist(err) {
			return errors.New("映射文件已存在或不可访问，请重新加载")
		}
		err = exclusiveFile(s.path, s.updated)
	}
	if err != nil {
		return errors.New("无法保存映射文件，请检查路径和权限")
	}
	s.committed = true
	return nil
}
func (s *mappingFileSave) rollback() error {
	if s == nil || !s.committed {
		return nil
	}
	current, err := regularRead(s.path, proxyconfig.MaxBytes)
	if err != nil || digest(current) != digest(s.updated) {
		return errors.New("映射文件已变化，无法自动回滚，请检查备份")
	}
	if s.existed {
		err = atomicFile(s.path, s.original)
	} else {
		err = os.Remove(s.path)
	}
	return err
}

// Stage beside the chosen JSON and validate against it before touching either
// live file. The .cf and JSON are then committed under the management write lock.
func (h *Handler) prepareMappingFile(current []byte, edits map[string]string, mappings []proxyMapping) (string, *mappingFileSave, error) {
	_, rawPath, err := mappingConfig(current)
	if err != nil {
		return "", nil, err
	}
	if value, ok := edits["proxies-file"]; ok {
		rawPath = value
	}
	if rawPath == "" {
		content, err := updateMappingFields(current, edits, mappings)
		return content, nil, err
	}
	path := proxyconfig.Path(h.view.AppConfigPath, rawPath)
	for _, dir := range []string{h.cfg.WebDir, h.cfg.SessionDir} {
		if dir != "" {
			relative, err := filepath.Rel(proxyconfig.Path("", dir), path)
			if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				return "", nil, errors.New("映射文件不能放在 Web 页面或登录会话目录中")
			}
		}
	}
	protected := []string{h.view.AppConfigPath, h.view.AppConfigPath + ".bak", h.cfg.UsersFile, h.cfg.SetupTokenFile}
	fields, _ := configFields(string(current))
	for _, f := range fields {
		if f.Key == "key-file" || f.Key == "tls-ca" || strings.HasPrefix(f.Key, "admin_") {
			protected = append(protected, proxyconfig.Path(h.view.AppConfigPath, f.Value))
		}
	}
	for _, p := range protected {
		if path == proxyconfig.Path("", p) || path+".bak" == proxyconfig.Path("", p) {
			return "", nil, errors.New("映射文件不能覆盖程序配置、密钥或管理存储文件")
		}
	}
	previous, _, loadErr := h.savedMappings(current)
	if loadErr != nil {
		return "", nil, loadErr
	}
	mappings, err = restoreMappingPasswords(mappings, previous)
	if err != nil {
		return "", nil, err
	}
	raw, err := encodeProxyMappings(mappings)
	if err != nil {
		return "", nil, err
	}
	if raw == "" {
		raw = "[]"
	}
	var formatted any
	_ = json.Unmarshal([]byte(raw), &formatted)
	data, _ := json.MarshalIndent(formatted, "", "  ")
	data = append(data, '\n')
	save := &mappingFileSave{path: path, configured: rawPath, updated: data}
	save.original, err = regularRead(path, proxyconfig.MaxBytes)
	save.existed = err == nil
	if err != nil && !os.IsNotExist(err) {
		return "", nil, errors.New("无法读取映射文件，请检查路径和权限")
	}
	candidate, err := os.CreateTemp(filepath.Dir(path), ".sgate-mappings-*.json")
	if err != nil {
		return "", nil, errors.New("无法创建待校验映射文件，请检查目录权限")
	}
	save.staged = candidate.Name()
	_, err = candidate.Write(data)
	if closeErr := candidate.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		save.cleanup()
		return "", nil, err
	}
	// The staging path is used only by the validator and listener synchronizer.
	copyEdits := map[string]string{}
	for k, v := range edits {
		if k != "proxies" {
			copyEdits[k] = v
		}
	}
	copyEdits["proxies-file"] = save.staged
	content, err := updateConfigFields(current, copyEdits)
	if err != nil {
		save.cleanup()
		return "", nil, err
	}
	return content, save, nil
}

func (h *Handler) savedMappingsMustRead() ([]proxyMapping, string, error) {
	content, err := regularRead(h.view.AppConfigPath, 1<<20)
	if err != nil {
		return nil, "", errors.New("无法读取客户端配置")
	}
	return h.savedMappings(content)
}
