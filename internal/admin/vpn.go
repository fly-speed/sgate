package admin

import (
	"errors"
	"fmt"
	"net/http"
	"sgate/internal/vpn"
	"slices"
	"strconv"
	"strings"
)

func vpnConfigFields(fields []configField, role string) []configField {
	fields = slices.DeleteFunc(fields, func(f configField) bool { return f.Key == "vpn-client-ip" })
	known := map[string]bool{}
	for _, f := range fields {
		known[f.Key] = true
	}
	for _, s := range vpn.Settings(role) {
		if !known[s.Key] {
			fields = append(fields, configField{Key: s.Key, Value: s.Default, Description: s.Description, Group: "独立 VPN", Validation: ruleForConfig(s.Key)})
		}
	}
	return fields
}
func prepareVPNConfigEdits(current []byte, edits map[string]string, role string) ([]byte, error) {
	fields, err := configFields(string(current))
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, f := range fields {
		known[f.Key] = true
	}
	entries := []string{}
	for _, s := range vpn.Settings(role) {
		if _, ok := edits[s.Key]; ok && !known[s.Key] {
			entries = append(entries, "    # VPN configuration managed through the Web interface; see docs/vpn.md.", "    "+s.Key+" = "+strconv.Quote(s.Default))
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

func configuredVPNOnly(content []byte) bool {
	fields, err := configFields(string(content))
	if err != nil {
		return false
	}
	for _, f := range fields {
		if f.Key == "vpn-mode" {
			return f.Value == "only"
		}
	}
	return false
}

func (h *Handler) vpnOnly(content []byte) bool {
	if h.view.Runtime != nil {
		if runtime, ok := h.view.Runtime().(map[string]any); ok {
			if overrides, ok := runtime["cli_overrides"].([]string); ok {
				for _, key := range overrides {
					if key == "vpn-mode" {
						return runtime["vpn_mode"] == "only"
					}
				}
			}
		}
	}
	return configuredVPNOnly(content)
}

func (h *Handler) configValidationProblem(w http.ResponseWriter, err error, content string) {
	message := "配置校验失败：" + err.Error()
	fieldErrors := map[string]string{}
	var invalid *vpn.ValidationError
	if errors.As(err, &invalid) {
		for _, key := range invalid.Fields {
			fieldErrors[key] = invalid.Message
		}
		message = "配置校验失败：" + invalid.Message
	}
	var unsupported interface{ OptionName() string }
	if errors.As(err, &unsupported) {
		other := "client"
		roleName, otherName := "服务端", "客户端"
		if h.view.Role == "client" {
			other = "server"
			roleName, otherName = otherName, roleName
		}
		own, foreign := map[string]bool{}, map[string]bool{}
		for _, s := range vpn.Settings(h.view.Role) {
			own[s.Key] = true
		}
		for _, s := range vpn.Settings(other) {
			if !own[s.Key] {
				foreign[s.Key] = true
			}
		}
		if other == "client" {
			foreign["vpn-client-ip"] = true
		}
		if foreign[unsupported.OptionName()] {
			fields, _ := configFields(content)
			keys := []string{}
			for _, f := range fields {
				if foreign[f.Key] {
					keys = append(keys, f.Key)
				}
			}
			reason := fmt.Sprintf("无法启用 VPN：%s配置包含仅适用于%s的参数：%s。请移除这些参数并使用本端 VPN 配置。", roleName, otherName, strings.Join(keys, ", "))
			fieldErrors["vpn-mode"] = reason
			for _, key := range keys {
				fieldErrors[key] = reason
			}
			message = reason
		}
	}
	reply(w, http.StatusBadRequest, map[string]any{"error": message, "field_errors": fieldErrors})
}
