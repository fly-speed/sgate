// Package serviceconfig reads a single acl_master service with the go-service
// configuration API. Validation supplements its permissive reader so malformed
// edits cannot silently remove settings, and quoted secrets retain literal #.
package serviceconfig

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	master "github.com/acl-dev/go-service"
)

func Read(path string) (values map[string]string, merged bool, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false, fmt.Errorf("read configuration: %w", err)
	}
	if len(b) > 1<<20 || !utf8.Valid(b) {
		return nil, false, fmt.Errorf("configuration must be UTF-8 and at most 1 MiB")
	}
	text := strings.TrimPrefix(string(b), "\uFEFF")
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		merged = strings.HasPrefix(line, "service ") && strings.HasSuffix(line, "{")
		break
	}
	if !merged {
		return nil, false, nil
	}
	// InitConfig is provided by the TCP template and shared by gin-service.
	// It panics on I/O errors; convert these to a safe configuration error.
	defer func() {
		if recover() != nil {
			values = nil
			err = fmt.Errorf("cannot read service configuration")
		}
	}()
	cfg := new(master.Config)
	cfg.InitConfig(path)
	values = make(map[string]string)
	open, closed := false, false
	scan := bufio.NewScanner(strings.NewReader(text))
	lineNo := 0
	for scan.Scan() {
		lineNo++
		line := strings.TrimSpace(scan.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if !open {
			if !strings.HasPrefix(line, "service ") || !strings.HasSuffix(line, "{") {
				return nil, true, fmt.Errorf("invalid service header")
			}
			open = true
			continue
		}
		if line == "}" && !closed {
			closed = true
			continue
		}
		if closed || len(scan.Text()) > 4095 {
			return nil, true, fmt.Errorf("configuration line %d: invalid service structure or oversized line", lineNo)
		}
		key, raw, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		raw = strings.TrimSpace(raw)
		if !ok || key == "" || strings.ContainsAny(key, " \t{}") {
			return nil, true, fmt.Errorf("configuration line %d: expected key = value", lineNo)
		}
		if _, ok := values[key]; ok {
			return nil, true, fmt.Errorf("configuration line %d: duplicate option", lineNo)
		}
		value, err := decodeServiceValue(raw, cfg.GetString(key), lineNo)
		if err != nil {
			return nil, true, err
		}
		values[key] = value
	}
	if scan.Err() != nil || !closed {
		return nil, true, fmt.Errorf("incomplete service configuration")
	}
	return values, true, nil
}

func FrameworkKey(key string) bool {
	return strings.HasPrefix(key, "master_") || strings.HasPrefix(key, "app_") || key == "tls_cert_file" ||
		key == "tls_key_file"
}

// decodeServiceValue 从原始带引号文本恢复值，保留密码或路径中的 #。
// 未带引号的值仍由 go-service 读取，维持模板的注释解析规则。
func decodeServiceValue(raw, value string, lineNo int) (string, error) {
	var err error
	if strings.HasPrefix(raw, "\"") {
		// The template strips inline comments without recognizing quoted strings.
		// Decode from the original quoted literal to preserve passwords and paths.
		end := 1
		escaped := false
		for ; end < len(raw); end++ {
			if raw[end] == '"' && !escaped {
				break
			}
			if raw[end] == '\\' && !escaped {
				escaped = true
			} else {
				escaped = false
			}
		}
		if end >= len(raw) {
			return "", fmt.Errorf("configuration line %d: invalid quoted value", lineNo)
		}
		tail := strings.TrimSpace(raw[end+1:])
		if tail != "" && !strings.HasPrefix(tail, "#") {
			return "", fmt.Errorf("configuration line %d: invalid quoted value", lineNo)
		}
		value, err = strconv.Unquote(raw[:end+1])
		if err != nil {
			return "", fmt.Errorf("configuration line %d: invalid quoted value", lineNo)
		}
	} else if strings.HasPrefix(raw, "'") {
		end := strings.Index(raw[1:], "'")
		if end < 0 {
			return "", fmt.Errorf("configuration line %d: invalid quoted value", lineNo)
		}
		end++
		tail := strings.TrimSpace(raw[end+1:])
		if tail != "" && !strings.HasPrefix(tail, "#") {
			return "", fmt.Errorf("configuration line %d: invalid quoted value", lineNo)
		}
		value = raw[1:end]
	}
	return value, nil
}
