package admin

import (
	"sgate/internal/vpn"
	"strconv"
	"strings"
)

func vpnConfigFields(fields []configField, role string) []configField {
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
			entries = append(entries, "    # "+s.Description, "    "+s.Key+" = "+strconv.Quote(s.Default))
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
