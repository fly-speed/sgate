package cli

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"sgate/internal/serviceconfig"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Parse applies defaults, file settings, then explicitly supplied flags.
// Register all application flags before calling Parse.
func Parse(fs *flag.FlagSet, args []string) error {
	path := fs.String("config", "", "application configuration file (command-line flags override file settings)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments; use named flags")
	}
	if *path == "" {
		if f := fs.Lookup("f"); f != nil && f.Value.String() != "" {
			*path = f.Value.String()
		} else {
			return nil
		}
	}
	explicit := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	values, merged, err := serviceconfig.Read(*path)
	if err != nil {
		return err
	}
	if merged {
		return applyService(fs, values, "", explicit)
	}
	f, err := os.Open(*path)
	if err != nil {
		return fmt.Errorf("read configuration: %w", err)
	}
	defer f.Close()
	seen := make(map[string]bool)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		text := scanner.Text()
		if line == 1 {
			text = strings.TrimPrefix(text, "\uFEFF")
		}
		if !utf8.ValidString(text) {
			return fmt.Errorf("configuration line %d: UTF-8 required", line)
		}
		text = strings.TrimSpace(text)
		if text == "" || strings.HasPrefix(text, "#") || strings.HasPrefix(text, ";") {
			continue
		}
		name, value, ok := strings.Cut(text, "=")
		if !ok {
			return fmt.Errorf("configuration line %d: expected key = value", line)
		}
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if name == "config" || fs.Lookup(name) == nil {
			return fmt.Errorf("configuration line %d: unknown or unsupported option", line)
		}
		if seen[name] {
			return fmt.Errorf("configuration line %d: duplicate option %q", line, name)
		}
		seen[name] = true
		value, err = configValue(value)
		if err != nil {
			return fmt.Errorf("configuration line %d: invalid quoted value for %q", line, name)
		}
		if !explicit[name] {
			if err := fs.Set(name, value); err != nil {
				// Flag conversion errors include the value; never echo secrets.
				return fmt.Errorf("configuration line %d: invalid value for %q", line, name)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read configuration: %w", err)
	}
	return nil
}

func configValue(value string) (string, error) {
	if strings.HasPrefix(value, "\"") {
		return strconv.Unquote(value)
	}
	if strings.HasPrefix(value, "'") {
		if len(value) < 2 || !strings.HasSuffix(value, "'") {
			return "", fmt.Errorf("unterminated quote")
		}
		return value[1 : len(value)-1], nil
	}
	return value, nil
}

// ParseSection reads management keys in the same service block, independently
// of the proxy flags. Legacy standalone management files remain supported.
func ParseSection(fs *flag.FlagSet, path, prefix string) error {
	values, merged, err := serviceconfig.Read(path)
	if err != nil {
		return err
	}
	if !merged {
		return Parse(fs, []string{"-config", path})
	}
	return applyService(fs, values, prefix, nil)
}
func applyService(fs *flag.FlagSet, values map[string]string, prefix string, explicit map[string]bool) error {
	for key, value := range values {
		name := key
		if prefix != "" {
			if !strings.HasPrefix(key, prefix) || key == "admin_enabled" {
				continue
			}
			name = strings.ReplaceAll(strings.TrimPrefix(key, prefix), "_", "-")
		} else {
			if serviceconfig.FrameworkKey(key) || (strings.HasPrefix(key, "admin_") && key != "admin_enabled") {
				continue
			}
			if key == "admin_enabled" {
				name = "admin-enabled"
			}
		}
		if name == "config" || fs.Lookup(name) == nil {
			return fmt.Errorf("unknown or unsupported service option")
		}
		if !explicit[name] {
			if err := fs.Set(name, value); err != nil {
				return fmt.Errorf("invalid service option %q", name)
			}
		}
	}
	return nil
}
