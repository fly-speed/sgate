package admin

import "strings"

// Older client deployments may predate username. Expose both credentials even
// when absent, but only add missing keys to disk when they are explicitly saved.
func clientCredentialFields(fields []configField) []configField {
	for _, key := range []string{"username", "password"} {
		index := -1
		for i := range fields {
			if fields[i].Key == key {
				index = i
				break
			}
		}
		if index < 0 {
			fields = append(fields, configField{Key: key, Validation: ruleForConfig(key)})
			index = len(fields) - 1
		}
		fields[index].Group = "服务端连接认证"
		fields[index].Secret = key == "password"
		if key == "username" {
			fields[index].Description = "连接服务端的隧道账号；填写服务端客户端账号列表中的用户名。留空使用旧单密码模式，与 Web 管理员账号无关。"
		} else {
			fields[index].Description = "连接服务端的公共隧道密码；未设置独立认证的映射使用此账号密码。未修改时保留原密码，修改后需应用代理配置。"
		}
	}
	return fields
}

func prepareClientConfigEdits(current []byte, edits map[string]string) ([]byte, error) {
	fields, err := configFields(string(current))
	if err != nil {
		return nil, err
	}
	known := make(map[string]bool, len(fields))
	for _, field := range fields {
		known[field.Key] = true
	}
	var entries []string
	for _, key := range []string{"username", "password", "proxies-file"} {
		if _, edited := edits[key]; edited && !known[key] {
			entries = append(entries, "    # Client setting managed through the Web interface.", "    "+key+" = ''")
		}
	}
	if len(entries) == 0 {
		return current, nil
	}
	lines := strings.Split(string(current), "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == "}" {
			lines = append(lines[:i], append(entries, lines[i:]...)...)
			return []byte(strings.Join(lines, "\n")), nil
		}
	}
	return []byte(string(current) + "\n" + strings.Join(entries, "\n") + "\n"), nil
}
