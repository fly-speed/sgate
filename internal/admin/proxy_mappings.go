package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"sgate/internal/proxyconfig"
)

// ProxyMapping 是表单中的一个映射对；JSON 配置的编码只在服务端进行。
type ProxyMapping struct {
	Listen   string  `json:"listen"`
	Server   string  `json:"server"`
	Addr     string  `json:"addr,omitempty"`
	Username *string `json:"username,omitempty"`
	Password *string `json:"password,omitempty"`
}

// Keep internal configuration helpers using the same mapping representation.
type proxyMapping = ProxyMapping

func proxyMappings(raw string) ([]proxyMapping, error) {
	mappings := []proxyMapping{}
	if strings.TrimSpace(raw) != "" {
		parsed, err := proxyconfig.Decode([]byte(raw))
		if err != nil {
			return nil, errors.New("代理映射格式错误，请检查 JSON、用户名和密码")
		}
		for _, m := range parsed {
			mappings = append(mappings, proxyMapping{Listen: m.Listen, Server: m.Server, Addr: m.Addr, Username: m.Username, Password: m.Password})
		}
	}
	for i := range mappings {
		mapping := &mappings[i]
		endpoint, err := url.Parse(mapping.Server)
		if err != nil {
			continue
		}
		query := endpoint.Query()
		if (mapping.Addr != "" && query.Has("addr")) || len(query["addr"]) == 1 {
			// 独立 addr 覆盖 URL 的旧值；表单只保留一个有效后端地址。
			// 否则用户清空后端字段后，旧 URL 的 addr 会意外重新生效。
			if mapping.Addr == "" {
				mapping.Addr = query.Get("addr")
			}
			query.Del("addr")
			endpoint.RawQuery = query.Encode()
			mapping.Server = endpoint.String()
		}
	}
	return mappings, nil
}

func encodeProxyMappings(mappings []proxyMapping) (string, error) {
	if len(mappings) > 64 {
		return "", errors.New("代理映射最多支持 64 项")
	}
	if len(mappings) == 0 {
		return "", nil
	}
	seen := map[int]bool{}
	for i := range mappings {
		m := &mappings[i]
		if err := proxyconfig.Credentials(proxyconfig.Mapping{Username: m.Username, Password: m.Password}); err != nil {
			return "", err
		}
		m.Listen, m.Server, m.Addr = strings.TrimSpace(m.Listen), strings.TrimSpace(m.Server), strings.TrimSpace(m.Addr)
		_, port, err := net.SplitHostPort(m.Listen)
		number, portErr := net.LookupPort("tcp", port)
		if err != nil || portErr != nil || number <= 0 || number > 65535 {
			return "", fmt.Errorf("第 %d 个映射的本地监听地址必须为 主机:端口，端口范围 1–65535", i+1)
		}
		if seen[number] {
			return "", fmt.Errorf("第 %d 个映射的本地监听端口重复", i+1)
		}
		seen[number] = true
		u, err := url.Parse(m.Server)
		if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Host == "" || u.User != nil || u.Fragment != "" {
			return "", fmt.Errorf("第 %d 个映射的服务端地址必须为有效的 ws:// 或 wss:// URL", i+1)
		}
		if m.Addr != "" {
			host, port, err := net.SplitHostPort(m.Addr)
			number, portErr := strconv.Atoi(port)
			if err != nil || host == "" || portErr != nil || number <= 0 || number > 65535 {
				return "", fmt.Errorf("第 %d 个映射的后端地址必须为 IP或域名:端口", i+1)
			}
		}
		if strings.ContainsAny(m.Listen+m.Server+m.Addr, "\r\n\x00") {
			return "", errors.New("代理映射不能包含换行或空字符")
		}
	}
	data, err := json.Marshal(mappings)
	if len(data) > proxyconfig.MaxBytes {
		return "", errors.New("proxy mappings exceed 1 MiB")
	}
	return string(data), err
}

func updateMappingFields(current []byte, fields map[string]string, mappings []proxyMapping) (string, error) {
	if _, exists := fields["proxies"]; exists {
		return "", errors.New("不能同时提交原始 proxies 值和映射表单")
	}
	previous, _, err := mappingConfig(current)
	if err != nil {
		return "", err
	}
	mappings, err = restoreMappingPasswords(mappings, previous)
	if err != nil {
		return "", err
	}
	value, err := encodeProxyMappings(mappings)
	if err != nil {
		return "", err
	}
	parsed, err := configFields(string(current))
	if err != nil {
		return "", err
	}
	found := false
	for _, field := range parsed {
		if field.Key == "proxies" {
			found = true
		}
	}
	if !found {
		// 旧配置也能首次添加映射；合并 .cf 中必须插入 service 块内部。
		lines := strings.Split(string(current), "\n")
		inserted := false
		for i, line := range lines {
			if strings.TrimSpace(line) == "}" {
				lines = append(lines[:i], append([]string{"    proxies ="}, lines[i:]...)...)
				inserted = true
				break
			}
		}
		if !inserted {
			lines = append(lines, "proxies =")
		}
		current = []byte(strings.Join(lines, "\n"))
	}
	edits := make(map[string]string, len(fields)+1)
	for key, value := range fields {
		edits[key] = value
	}
	edits["proxies"] = value
	return updateConfigFields(current, edits)
}
