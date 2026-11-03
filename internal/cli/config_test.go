package cli

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func configFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sgate.conf")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConfigPrecedence(t *testing.T) {
	for _, mode := range []string{"defaults", "file", "command-line"} {
		t.Run(mode, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			listen := fs.String("listen", "default", "")
			password := fs.String("password", "default-password", "")
			interval := fs.Duration("noise-interval", 0, "")
			count := fs.Int("count", 0, "")
			enabled := fs.Bool("enabled", false, "")
			var args []string
			if mode != "defaults" {
				path := configFile(
					t,
					("listen = file\n" +
						"password = 'file # password=a'\n" +
						"noise-interval = 3s\n" +
						"count = 8\n" +
						"enabled = true\n"),
				)
				args = []string{"-config", path}
			}
			if mode == "command-line" {
				// Values equal to defaults, zero, false and empty are still explicit.
				args = append([]string{"-listen", "default", "-password=", "-noise-interval=0", "-count=0", "-enabled=false"}, args...)
			}
			if err := Parse(fs, args); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "defaults":
				if *listen != "default" || *password != "default-password" || *interval != 0 || *count != 0 || *enabled {
					t.Fatal("defaults changed")
				}
			case "file":
				if *listen != "file" || *password != "file # password=a" || *interval != 3*time.Second || *count != 8 ||
					!*enabled {
					t.Fatal("file values were not applied")
				}
			case "command-line":
				if *listen != "default" || *password != "" || *interval != 0 || *count != 0 || *enabled {
					t.Fatal("explicit command-line values did not win")
				}
			}
		})
	}
}

func TestConfigErrorsDoNotExposeValues(t *testing.T) {
	for name, content := range map[string]string{
		"unknown":       "typo = private-value",
		"unknown name":  "private-value = value",
		"duplicate":     "password = private-value\npassword = again",
		"syntax":        "private-value",
		"duration":      "heartbeat = private-value",
		"quoted":        "password = \"private-value",
		"nested config": "config = private-value",
		"encoding":      "password = private-value\xff",
	} {
		t.Run(name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			fs.String("password", "", "")
			fs.Duration("heartbeat", time.Second, "")
			err := Parse(fs, []string{"-config", configFile(t, content)})
			if err == nil || strings.Contains(err.Error(), "private-value") {
				t.Fatalf("expected safe configuration error, got %v", err)
			}
		})
	}
}

func TestCommandLineReplacesInvalidFileValue(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	heartbeat := fs.Duration("heartbeat", time.Second, "")
	if err := Parse(fs, []string{"-config", configFile(t, "heartbeat = invalid\n"), "-heartbeat", "20s"}); err != nil {
		t.Fatal(err)
	}
	if *heartbeat != 20*time.Second {
		t.Fatal("overridden file value was applied")
	}
}

func TestConfigTextAndHelp(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	password := fs.String("password", "", "")
	listen := fs.String("listen", "", "")
	path := configFile(t, ("\ufeff# comment\r\n" +
		"; comment\r\n" +
		"\r\n" +
		"password = \" a#=b\\\"c \"\r\n" +
		"listen = [::1]:2901\r\n"))
	if err := Parse(fs, []string{"-config=" + path}); err != nil {
		t.Fatal(err)
	}
	if *password != " a#=b\"c " || *listen != "[::1]:2901" {
		t.Fatal("quoted values, CRLF or BOM were not preserved")
	}
	fs = flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := Parse(fs, []string{"-config", "does-not-exist.conf", "-help"}); err != flag.ErrHelp {
		t.Fatalf("help unexpectedly read configuration: %v", err)
	}
}

func TestConfigMissingFileAndPositionalArguments(t *testing.T) {
	for _, args := range [][]string{{"-config", filepath.Join(t.TempDir(), "missing.conf")}, {"unexpected"}} {
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		if err := Parse(fs, args); err == nil {
			t.Fatal("invalid invocation accepted")
		}
	}
}

func TestUnknownServiceOptionReportsLineWithoutExposingKeyOrValue(t *testing.T) {
	for _, section := range []bool{false, true} {
		fs := flag.NewFlagSet("service", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fs.String("listen", "", "")
		body := "service example {\n # option follows\n pasted-private-key = private-value\n}\n"
		prefix := ""
		if section {
			body = strings.ReplaceAll(body, "pasted-private-key", "admin_pasted-private-key")
			prefix = "admin_"
		}
		path := configFile(t, body)
		var err error
		if section {
			err = ParseSection(fs, path, prefix)
		} else {
			err = Parse(fs, []string{"-config", path})
		}
		if err == nil || !strings.Contains(err.Error(), "configuration line 3:") || strings.Contains(err.Error(), "pasted-private-key") || strings.Contains(err.Error(), "private-value") {
			t.Fatalf("unsafe or unhelpful error: %v", err)
		}
	}
}
