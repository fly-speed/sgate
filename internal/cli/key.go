package cli

import (
	"errors"
	"os"
	"strings"

	"sgate/internal/tunnel"
)

// NormalizeKeyFilePath handles empty values passed as literal quotes by a
// service launcher or saved as text in the management UI.
func NormalizeKeyFilePath(path string) string {
	if path == "''" || path == "\"\"" {
		return ""
	}
	return path
}

func LoadKey(path string) ([]byte, error) {
	path = NormalizeKeyFilePath(path)
	value := os.Getenv("SGATE_KEY")
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		value = strings.TrimSpace(string(b))
	}
	if value == "" {
		return nil, errors.New("set SGATE_KEY or provide -key-file (generate with: openssl rand -hex 32)")
	}
	return tunnel.ParseKey(value)
}
