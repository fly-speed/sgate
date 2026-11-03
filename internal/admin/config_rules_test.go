package admin

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestEveryApplicationConfigFieldHasValidationAndRoundTrips(t *testing.T) {
	paths, err := filepath.Glob("../../*.cf")
	if err != nil || len(paths) == 0 {
		t.Fatal("application configurations not found")
	}
	samples, err := filepath.Glob("../../samples/*.cf")
	if err != nil {
		t.Fatal(err)
	}
	paths = append(paths, samples...)
	keys := map[string]bool{}
	var report []map[string]any
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		fields, err := configFields(string(data))
		if err != nil {
			t.Fatal(filepath.Base(path), err)
		}
		visible, err := configFields(redactConfig(data))
		if err != nil {
			t.Fatal(err)
		}
		role := "server"
		if strings.Contains(filepath.Base(path), "client") {
			role = "client"
		}
		for i := range visible {
			if visible[i].Key == "master_args" {
				visible[i].Validation.Options = argumentOptions(role)
			}
			if visible[i].Key == "proxies" {
				visible[i].Kind = "proxies"
				visible[i].Mappings, _ = proxyMappings(visible[i].Value)
				visible[i].Value = ""
			}
		}
		report = append(report, map[string]any{"name": filepath.Base(path), "role": role, "fields": visible})
		for _, field := range fields {
			keys[field.Key] = true
			rule, ok := configRules[field.Key]
			if !ok || rule.Hint == "" {
				t.Fatalf("%s: missing field rule for %s", filepath.Base(path), field.Key)
			}
			// 改为同类型的合法值，逐项核对写回、编码、注释和密码之外的字段均保留。
			value := exampleConfigValue(field)
			updated, err := updateConfigFields(data, map[string]string{field.Key: value})
			if err != nil {
				t.Fatalf("%s: cannot save %s: %v", filepath.Base(path), field.Key, err)
			}
			restored, err := configFields(updated)
			if err != nil {
				t.Fatal(err)
			}
			for _, after := range restored {
				if after.Key == field.Key && after.Value != value {
					t.Fatalf("%s: value did not round trip", field.Key)
				}
				if after.Key != field.Key {
					for _, before := range fields {
						if before.Key == after.Key && before.Value != after.Value {
							t.Fatalf("%s changed unrelated field %s", field.Key, after.Key)
						}
					}
				}
			}
		}
	}
	if path := os.Getenv("SGATE_CONFIG_RULES_REPORT"); path != "" {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("checked %d configuration files and %d distinct fields", len(paths), len(keys))
}

func exampleConfigValue(field configField) string {
	switch field.Validation.Type {
	case "duration":
		return "30s"
	case "bytes":
		return "2K"
	case "integer":
		if field.Key == "buffer-bytes" {
			return "65536"
		}
		if minimum, err := strconv.ParseInt(field.Validation.Min, 10, 64); err == nil && minimum > 1 {
			return field.Validation.Min
		}
		return "1"
	case "seconds":
		return "2"
	case "boolean", "enum":
		return field.Validation.Choices[0]
	case "listen":
		return "127.0.0.1:19001"
	case "address":
		return "localhost:19001"
	case "addresses":
		return "localhost:19001,[::1]:19002"
	case "listeners":
		return "127.0.0.1|19001"
	case "url":
		return "wss://localhost:19001/tunnel"
	case "route":
		return "/proxy-test"
	case "password":
		return "test-password-only"
	case "arguments":
		return "-noise-max-bytes 10M"
	case "file":
		return "test/config-path"
	case "proxies":
		return `[{"listen":"127.0.0.1:19001","server":"ws://localhost/tunnel"}]`
	default:
		return "test-value"
	}
}

func TestConfigFieldRulesRejectInvalidEditsWithoutChangingFile(t *testing.T) {
	cases := map[string]string{
		"noise-traffic": "maybe", "noise-idle": "yes", "noise-empty": "-1", "noise-idle-after": "-1s",
		"heartbeat": "1", "heartbeat-timeout": "-1s", "noise-interval": "1 second",
		"noise-min-bytes": "1.5K", "noise-max-bytes": "11M", "buffer-bytes": "4M",
		"max-streams": "-1", "admin_max_sessions": "10001", "admin_session_timeout": "500ms",
		"master_maxproc": "2", "master_wakeup": "10s", "send-addr": "maybe",
		"master_reuseport": "yes", "master_type": "udp", "listen": "localhost:65536",
		"target": ":80", "addr": "example.com:0", "allowed-targets": "localhost:80,",
		"server": "http://localhost/tunnel", "path": "/tunnel?addr=localhost:80",
	}
	for key, value := range cases {
		t.Run(key, func(t *testing.T) {
			data := []byte(key + " = " + exampleConfigValue(configField{Key: key, Validation: ruleForConfig(key)}) + "\n")
			if _, err := updateConfigFields(data, map[string]string{key: value}); err == nil {
				t.Fatalf("invalid edit accepted for %s", key)
			}
		})
	}
}

func TestConfigAPIIncludesUnitsAndRejectsTypedValueBeforeCommit(t *testing.T) {
	h, cfg := adminFixture(t)
	if err := os.WriteFile(h.view.AppConfigPath, []byte("noise-max-bytes = 10M\nadmin_session_timeout = 12h\nmaster_wakeup = -\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := setupAccount(t, h, cfg)
	get := object(t, request(h, "GET", "/api/config", nil, cookie, "", ""))
	raw, _ := json.Marshal(get["fields"])
	if !strings.Contains(string(raw), "1048576") || !strings.Contains(string(raw), "单位为秒") || !strings.Contains(string(raw), "12h") {
		t.Fatal("unit information not exposed")
	}
	before, _ := os.ReadFile(h.view.AppConfigPath)
	for _, bad := range []string{"11M", "bad", "-1"} {
		w := request(h, "PUT", "/api/config", map[string]any{"fields": map[string]string{"noise-max-bytes": bad}, "revision": get["revision"]}, cookie, csrf, "")
		after, _ := os.ReadFile(h.view.AppConfigPath)
		if w.Code != 400 || string(after) != string(before) {
			t.Fatal("invalid value modified configuration", w.Code)
		}
	}
	w := request(h, "PUT", "/api/config", map[string]any{"fields": map[string]string{"noise-max-bytes": "2M"}, "revision": get["revision"]}, cookie, csrf, "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestPublicCommandLineOptionsHaveFormRules(t *testing.T) {
	for _, role := range []string{"client", "server"} {
		tree, err := parser.ParseFile(token.NewFileSet(), "../../cmd/sgate-"+role+"/options.go", nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(tree, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			owner, ok := selector.X.(*ast.Ident)
			if !ok {
				return true
			}
			index := 0
			if owner.Name == "cli" && selector.Sel.Name == "ByteSizeVar" {
				index = 2
			} else if owner.Name == "flag" {
				switch selector.Sel.Name {
				case "String", "StringVar", "Bool", "BoolVar", "Int", "IntVar", "Duration", "DurationVar":
				default:
					return true
				}
				if strings.HasSuffix(selector.Sel.Name, "Var") {
					index = 1
				}
			} else {
				return true
			}
			if len(call.Args) <= index {
				return true
			}
			literal, ok := call.Args[index].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			name, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Fatal(err)
			}
			if _, exists := configRules[name]; !exists {
				t.Errorf("%s option %s has no form validation rule", role, name)
			}
			return true
		})
	}
}
