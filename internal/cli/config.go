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
func Parse(fs *flag.FlagSet, args []string) (result error) {
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
	// An explicitly supplied inline mapping (including empty) overrides a file
	// selected on disk. An explicit file option wins when both options are supplied.
	defer func() {
		if result == nil && explicit["proxies"] && !explicit["proxies-file"] && fs.Lookup("proxies-file") != nil {
			_ = fs.Set("proxies-file", "")
		}
	}()
	values, merged, err := serviceconfig.Read(*path)
	if err != nil {
		return err
	}
	if merged {
		return annotateServiceOptionError(applyService(fs, values, "", explicit), *path)
	}
	f, err := os.Open(*path)
	if err != nil {
		return fmt.Errorf("read configuration: %w", err)
	}
	defer f.Close()
	ignoreVPN := plainVPNDisabled(fs, *path, explicit)
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
		ignoredVPN := (ignoreVPN && inactiveVPNOption(name)) || ignoredFileOption(fs, name)
		if (name == "config" || fs.Lookup(name) == nil) && !ignoredVPN {
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
		if !ignoredVPN && !explicit[name] {
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
	return annotateServiceOptionError(applyService(fs, values, prefix, nil), path)
}
func applyService(fs *flag.FlagSet, values map[string]string, prefix string, explicit map[string]bool) error {
	ignoreVPN := prefix == "" && vpnDisabled(fs, values["vpn-mode"], explicit)
	for key, value := range values {
		if prefix == "" && ((ignoreVPN && inactiveVPNOption(key)) || ignoredFileOption(fs, key)) {
			continue
		}
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
			return &unsupportedServiceOption{key: key}
		}
		if !explicit[name] {
			if err := fs.Set(name, value); err != nil {
				return fmt.Errorf("invalid service option %q", name)
			}
		}
	}
	return nil
}

// Keep unknown option names and values private, but identify their source line.
// A malformed key may itself contain a pasted credential.
type unsupportedServiceOption struct{ key string }

func (e *unsupportedServiceOption) OptionName() string { return e.key }

func (*unsupportedServiceOption) Error() string { return "unknown or unsupported service option" }
func annotateServiceOptionError(err error, path string) error {
	option, ok := err.(*unsupportedServiceOption)
	if !ok {
		return err
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		return err
	}
	for i, line := range strings.Split(string(data), "\n") {
		text := strings.TrimSpace(strings.TrimPrefix(line, "\uFEFF"))
		if strings.HasPrefix(text, "#") || strings.HasPrefix(text, ";") {
			continue
		}
		key, _, found := strings.Cut(text, "=")
		if found && strings.TrimSpace(key) == option.key {
			return fmt.Errorf("configuration line %d: %w", i+1, err)
		}
	}
	return err
}

// Disabled optional VPN configuration must not prevent the proxy from starting.
// Apply this only to applications that actually register VPN mode; unrelated
// configuration consumers retain their strict unknown-option behavior.
func vpnDisabled(fs *flag.FlagSet, fileMode string, explicit map[string]bool) bool {
	mode := fs.Lookup("vpn-mode")
	if mode == nil {
		return false
	}
	if explicit["vpn-mode"] {
		fileMode = mode.Value.String()
	} else if fileMode == "" {
		fileMode = mode.DefValue
	}
	return fileMode == "disabled"
}
func inactiveVPNOption(name string) bool {
	return strings.HasPrefix(name, "vpn-") && name != "vpn-mode"
}

// IgnoreFileOption accepts a legacy file entry without applying its value.
// Passing it explicitly on the command line is still an error.
func IgnoreFileOption(fs *flag.FlagSet, name string) {
	fs.Var(legacyFileOption{}, name, "legacy configuration file entry; ignored")
}

type legacyFileOption struct{}

func (legacyFileOption) String() string { return "" }
func (legacyFileOption) Set(string) error {
	return fmt.Errorf("option is only accepted in legacy configuration files")
}

func ignoredFileOption(fs *flag.FlagSet, name string) bool {
	if f := fs.Lookup(name); f != nil {
		_, ok := f.Value.(legacyFileOption)
		return ok
	}
	return false
}
func plainVPNDisabled(fs *flag.FlagSet, path string, explicit map[string]bool) bool {
	if fs.Lookup("vpn-mode") == nil {
		return false
	}
	if explicit["vpn-mode"] {
		return vpnDisabled(fs, "", explicit)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, raw, ok := strings.Cut(strings.TrimPrefix(line, "\uFEFF"), "=")
		if ok && strings.TrimSpace(key) == "vpn-mode" {
			mode, err := configValue(strings.TrimSpace(raw))
			if err != nil {
				return false
			}
			return vpnDisabled(fs, mode, explicit)
		}
	}
	return vpnDisabled(fs, "", explicit)
}

// ReadValues decodes file syntax without applying flag types or dormant-mode
// filtering. Callers still validate selected values and command-line precedence.
func ReadValues(path string) (map[string]string, error) {
	if path == "" {
		return map[string]string{}, nil
	}
	values, merged, err := serviceconfig.Read(path)
	if err != nil || merged {
		return values, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	values = map[string]string{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		text := scanner.Text()
		if line == 1 {
			text = strings.TrimPrefix(text, "\uFEFF")
		}
		if !utf8.ValidString(text) {
			return nil, fmt.Errorf("configuration line %d: UTF-8 required", line)
		}
		text = strings.TrimSpace(text)
		if text == "" || strings.HasPrefix(text, "#") || strings.HasPrefix(text, ";") {
			continue
		}
		key, value, ok := strings.Cut(text, "=")
		if !ok {
			return nil, fmt.Errorf("configuration line %d: expected key = value", line)
		}
		key = strings.TrimSpace(key)
		if _, exists := values[key]; exists {
			return nil, fmt.Errorf("configuration line %d: duplicate option", line)
		}
		value, err = configValue(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("configuration line %d: invalid quoted value", line)
		}
		values[key] = value
	}
	return values, scanner.Err()
}
