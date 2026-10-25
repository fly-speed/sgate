package serviceconfig_test

import (
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"sgate/internal/admin"
	"sgate/internal/cli"
	"sgate/internal/serviceconfig"
)

func file(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "service.cf")
	if e := os.WriteFile(path, []byte(text), 0600); e != nil {
		t.Fatal(e)
	}
	return path
}
func TestMergedTemplateConfigAndPrecedence(t *testing.T) {
	path := file(
		t,
		("service test {\n" +
			" master_service = 127.0.0.1|9000\n" +
			" master_args =\n" +
			" listen = file\n" +
			" password = \" a # secret=ok \\\" \\\\ \" # comment\n" +
			" noise-interval-min = 1s\n" +
			" noise-interval-max = 3s\n" +
			" admin_enabled = true\n" +
			" admin_listen = 127.0.0.1:9082\n" +
			" admin_web_dir = web/admin\n" +
			" admin_session_timeout = 2h\n" +
			"}\n"),
	)
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.String("f", "", "")
	listen := fs.String("listen", "default", "")
	password := fs.String("password", "", "")
	min := fs.Duration("noise-interval-min", 0, "")
	max := fs.Duration("noise-interval-max", 0, "")
	enabled := fs.Bool("admin-enabled", false, "")
	if e := cli.Parse(fs, []string{"-f", path, "-listen", "override", "-noise-interval-min=0", "-noise-interval-max=0"}); e != nil {
		t.Fatal(e)
	}
	if *listen != "override" || *password != " a # secret=ok \" \\ " || *min != 0 || *max != 0 || !*enabled ||
		fs.Lookup("config").Value.String() != path {
		t.Fatal("merged precedence or quoted secret changed")
	}
	cfg, e := admin.LoadConfig(path)
	if e != nil {
		t.Fatal(e)
	}
	if cfg.Listen != "127.0.0.1:9082" || cfg.SessionTimeout != 2*time.Hour ||
		cfg.WebDir != filepath.Join(filepath.Dir(path), "web/admin") {
		t.Fatal("management namespace or relative paths changed")
	}
}
func TestMergedInvalidEditsAreSafe(t *testing.T) {
	for _, text := range []string{
		"service x {\n password = private-value\n password = again\n}\n",
		"service x {\npassword = private-value\n",
		"service x {\npassword = \"private-value\n}\n",
		"service x {\npassword = private-value\n}\nservice y {\n}\n",
		"service x {\npassword = " + strings.Repeat("private-value", 400) + "\n}\n",
	} {
		_, merged, e := serviceconfig.Read(file(t, text))
		if !merged || e == nil || strings.Contains(e.Error(), "private-value") {
			t.Fatal("invalid edit accepted or value exposed")
		}
	}
}

// 移动样例目录后，账号、会话和界面仍应解析到同一份项目资源。
func TestShippedConfigManagementPaths(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, pattern := range []string{"*.cf", "samples/*.cf"} {
		paths, err := filepath.Glob(filepath.Join(root, pattern))
		if err != nil || len(paths) == 0 {
			t.Fatalf("missing configurations: %s, %v", pattern, err)
		}
		for _, path := range paths {
			if _, merged, err := serviceconfig.Read(path); err != nil || !merged {
				t.Fatalf("invalid service configuration %s: %v", path, err)
			}
			cfg, err := admin.LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			role := "client"
			if strings.Contains(filepath.Base(path), "server") {
				role = "server"
			}
			if cfg.UsersFile != filepath.Join(root, "conf/admin-"+role+"-users.json") ||
				cfg.SessionDir != filepath.Join(root, "conf/admin-"+role+"-sessions") ||
				cfg.WebDir != filepath.Join(root, "web/admin") {
				t.Fatalf("management resources moved for %s", path)
			}
		}
	}
}

// The private, disposable baseline is captured immediately before migration;
// the test never prints credential values or embeds them in the repository.
func TestShippedMigrationPreservesValues(t *testing.T) {
	baseline := os.Getenv("SGATE_CONFIG_MIGRATION_BASELINE")
	if baseline == "" {
		t.Skip("private pre-migration snapshot required")
	}
	b, e := os.ReadFile(baseline)
	if e != nil {
		t.Fatal(e)
	}
	var old map[string]string
	if e = json.Unmarshal(b, &old); e != nil {
		t.Fatal(e)
	}
	root, e := filepath.Abs("../..")
	if e != nil {
		t.Fatal(e)
	}
	for name, text := range old {
		if strings.HasPrefix(name, "admin-") {
			continue
		}
		path := filepath.Join(root, strings.TrimSuffix(name, ".conf")+".cf")
		if _, err := os.Stat(path); os.IsNotExist(err) {
			path = filepath.Join(root, "samples", filepath.Base(path))
		}
		values, merged, e := serviceconfig.Read(path)
		if e != nil || !merged {
			t.Fatalf("read %s: %v", name, e)
		}
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			key, value, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			key = strings.TrimSpace(key)
			value = strings.TrimSpace(value)
			if strings.HasPrefix(value, "\"") {
				value, e = strconv.Unquote(value)
				if e != nil {
					t.Fatal("baseline quote")
				}
			}
			if strings.HasPrefix(value, "'") {
				value = strings.Trim(value, "'")
			}
			if values[key] != value {
				t.Fatalf("%s option %s changed", name, key)
			}
		}
		role := "client"
		if strings.Contains(name, "server") {
			role = "server"
		}
		cfg, e := admin.LoadConfig(path)
		if e != nil {
			t.Fatal(e)
		}
		if cfg.UsersFile != filepath.Join(root, "conf/admin-"+role+"-users.json") ||
			cfg.SessionDir != filepath.Join(root, "conf/admin-"+role+"-sessions") ||
			cfg.WebDir != filepath.Join(root, "web/admin") {
			t.Fatal("management storage/UI path changed")
		}
		for _, line := range strings.Split(old["admin-"+role+".conf"], "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			key, value, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			key, value = strings.TrimSpace(key), strings.TrimSpace(value)
			switch key {
			case "web-dir":
				value = "web/admin"
			case "users-file", "session-dir", "setup-token-file":
				value = "conf/" + value
			}
			if values["admin_"+strings.ReplaceAll(key, "-", "_")] != value {
				t.Fatalf("%s management option %s changed", name, key)
			}
		}
		if values["admin_enabled"] != "false" {
			t.Fatal("management enabled by migration")
		}
		t.Logf("%s: application values and management paths preserved", name)
	}
}
