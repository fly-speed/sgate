package admin

import (
	"errors"
	"strconv"
	"strings"

	"sgate/internal/serviceconfig"
)

// configField 是表单视图；保存时仅替换修改项，保留原文件的注释和布局。
type configField struct {
	Key         string         `json:"key"`
	Value       string         `json:"value"`
	Description string         `json:"description"`
	Secret      bool           `json:"secret"`
	Group       string         `json:"group"`
	Kind        string         `json:"kind,omitempty"`
	Validation  configRule     `json:"validation"`
	Mappings    []proxyMapping `json:"mappings,omitempty"`
}

// fieldValue 分离值和行尾注释。旧式配置的未加引号值允许包含 #。
func fieldValue(raw string, merged bool) (string, string, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "\"") || strings.HasPrefix(raw, "'") {
		quote := raw[0]
		escaped := false
		for i := 1; i < len(raw); i++ {
			if raw[i] == quote && !escaped {
				value := raw[1:i]
				if quote == '"' {
					var err error
					value, err = strconv.Unquote(raw[:i+1])
					if err != nil {
						return "", "", err
					}
				}
				tail := strings.TrimSpace(raw[i+1:])
				if tail != "" && (!merged || !strings.HasPrefix(tail, "#")) {
					return "", "", errors.New("配置项引号后存在无效内容")
				}
				return value, tail, nil
			}
			if quote == '"' && raw[i] == '\\' && !escaped {
				escaped = true
			} else {
				escaped = false
			}
		}
		return "", "", errors.New("配置项引号不完整")
	}
	if merged {
		value, tail, found := strings.Cut(raw, "#")
		if found {
			return strings.TrimSpace(value), "#" + tail, nil
		}
	}
	return raw, "", nil
}

func configFields(content string) ([]configField, error) {
	fields := []configField{}
	seen := map[string]bool{}
	merged := false
	description := ""
	for _, line := range strings.Split(content, "\n") {
		text := strings.TrimSpace(strings.TrimPrefix(line, "\uFEFF"))
		if strings.HasPrefix(text, "#") || strings.HasPrefix(text, ";") {
			description = strings.TrimSpace(text[1:])
			continue
		}
		if strings.HasPrefix(text, "service ") && strings.HasSuffix(text, "{") {
			merged = true
			description = ""
			continue
		}
		key, raw, ok := strings.Cut(text, "=")
		if !ok {
			description = ""
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" || strings.ContainsAny(key, " \t{}") || seen[key] {
			return nil, errors.New("配置项名称无效或重复，请先检查配置文件")
		}
		seen[key] = true
		value, _, err := fieldValue(raw, merged)
		if err != nil {
			return nil, err
		}
		group := "代理与通道"
		if strings.HasPrefix(key, "vpn-") {
			group = "独立 VPN"
		}
		if serviceconfig.FrameworkKey(key) {
			group = "服务框架"
		} else if strings.HasPrefix(key, "admin_") || key == "admin-config" || key == "admin-enabled" {
			group = "Web 管理"
		}
		fields = append(fields, configField{Key: key, Value: value, Description: description, Secret: secretConfigKey(line) != "", Group: group, Validation: ruleForConfig(key)})
		description = ""
	}
	return fields, nil
}

func updateConfigFields(current []byte, edits map[string]string) (string, error) {
	fields, err := configFields(string(current))
	if err != nil {
		return "", err
	}
	known := make(map[string]configField, len(fields))
	for _, field := range fields {
		known[field.Key] = field
	}
	for key, value := range edits {
		if _, ok := known[key]; !ok {
			return "", errors.New("配置项已不存在，请重新加载")
		}
		if value != secretMarker {
			if err := validateConfigEdit(key, value); err != nil {
				return "", errors.New(key + "：" + err.Error())
			}
		}
		if strings.ContainsAny(value, "\r\n\x00") {
			return "", errors.New("配置值不能包含换行或空字符")
		}
	}
	lines := strings.Split(string(current), "\n")
	merged := false
	for i, line := range lines {
		text := strings.TrimSpace(strings.TrimPrefix(line, "\uFEFF"))
		if strings.HasPrefix(text, "service ") && strings.HasSuffix(text, "{") {
			merged = true
		}
		if strings.HasPrefix(text, "#") || strings.HasPrefix(text, ";") {
			continue
		}
		key, raw, ok := strings.Cut(text, "=")
		key = strings.TrimSpace(key)
		value, changed := edits[key]
		if !ok || !changed || value == known[key].Value || (known[key].Secret && value == secretMarker) {
			continue
		}
		_, tail, err := fieldValue(raw, merged)
		if err != nil {
			return "", err
		}
		encoded := strconv.Quote(value)
		// JSON 自带双引号；优先用单引号包裹，避免反复转义撑破模板的行长限制。
		if key == "proxies" && !strings.Contains(value, "'") {
			encoded = "'" + value + "'"
		}
		// 框架本身不使用应用层的引号解码，框架参数继续按原模板格式写入。
		if serviceconfig.FrameworkKey(key) {
			if strings.Contains(value, "#") || value != strings.TrimSpace(value) {
				return "", errors.New("框架配置值不能包含 # 或首尾空白")
			}
			encoded = value
		}
		if tail != "" {
			encoded += " " + tail
		}
		prefix, _, _ := strings.Cut(line, "=")
		ending := ""
		if strings.HasSuffix(line, "\r") {
			ending = "\r"
		}
		lines[i] = prefix + "= " + encoded + ending
	}
	return strings.Join(lines, "\n"), nil
}
