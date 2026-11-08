package cli

import (
	"bufio"
	"flag"
	"log"
	"os"
	"strings"

	"sgate/internal/serviceconfig"
)

// These flags identify the client even in independently cloned flag sets used
// for live validation. Server and standalone VPN parsing remain strict.
func clientFlags(fs *flag.FlagSet) bool {
	return fs.Lookup("proxies") != nil && fs.Lookup("proxies-file") != nil
}

func serviceOptionName(fs *flag.FlagSet, key, prefix string) (string, bool) {
	if prefix != "" {
		if !strings.HasPrefix(key, prefix) || key == "admin_enabled" {
			return "", true
		}
		return strings.ReplaceAll(strings.TrimPrefix(key, prefix), "_", "-"), false
	}
	if (serviceconfig.FrameworkKey(key) && !(key == "master_service" && fs.Lookup(key) != nil)) || (strings.HasPrefix(key, "admin_") && key != "admin_enabled") {
		return "", true
	}
	if key == "admin_enabled" {
		return "admin-enabled", false
	}
	return key, false
}

// LogClientConfigWarnings repeats startup warnings after go-service initializes
// master_log, so they reach sgate-client.log rather than only startup stderr.
func LogClientConfigWarnings(fs *flag.FlagSet) {
	if !clientFlags(fs) {
		return
	}
	path := fs.Lookup("config")
	if path == nil || path.Value.String() == "" {
		return
	}
	_, merged, err := serviceconfig.Read(path.Value.String())
	if err == nil {
		logUnknownOptions(fs, path.Value.String(), "", merged)
	}
}

func logUnknownOptions(fs *flag.FlagSet, path, prefix string, merged bool) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 4096), 1<<20)
	for line := 1; scan.Scan(); line++ {
		text := strings.TrimSpace(strings.TrimPrefix(scan.Text(), "\uFEFF"))
		if strings.HasPrefix(text, "#") || strings.HasPrefix(text, ";") {
			continue
		}
		key, _, ok := strings.Cut(text, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		name, ignored := key, false
		if merged {
			name, ignored = serviceOptionName(fs, key, prefix)
		}
		if !ignored && fs.Lookup(name) == nil {
			warnUnknownOption(path, line, key)
		}
	}
}

func warnUnknownOption(path string, line int, key string) {
	// Values are never logged. Malformed/oversized names may contain pasted
	// credentials; report only their source line instead of echoing raw text.
	safe := len(key) > 0 && len(key) <= 128
	for _, c := range key {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			safe = false
		}
	}
	if !safe {
		key = "<unrecognized key>"
	}
	log.Printf("WARNING: configuration %q line %d: unknown option %q ignored", path, line, key)
}
