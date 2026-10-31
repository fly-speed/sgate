// Package proxyconfig reads client mapping files without depending on the UI or tunnel.
package proxyconfig

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

const MaxBytes = 1 << 20

type Mapping struct {
	AllowReverse bool   `json:"allow_reverse,omitempty"`
	Listen       string `json:"listen"`
	Server       string `json:"server"`
	Addr         string `json:"addr,omitempty"`
	// Nil inherits the common credential; an explicit empty username selects legacy auth.
	Username *string `json:"username,omitempty"`
	Password *string `json:"password,omitempty"`
}

func Decode(data []byte) ([]Mapping, error) {
	if !utf8.Valid(data) {
		return nil, errors.New("proxy mappings require UTF-8")
	}
	if len(data) > MaxBytes {
		return nil, errors.New("proxy mappings exceed 1 MiB")
	}
	var mappings []Mapping
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&mappings); err != nil || mappings == nil {
		return nil, errors.New("proxy mappings must be a JSON array")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, errors.New("unexpected data after proxy mappings")
	}
	if len(mappings) > 64 {
		return nil, errors.New("proxy mappings allow at most 64 entries")
	}
	for _, m := range mappings {
		if err := Credentials(m); err != nil {
			return nil, err
		}
	}
	return mappings, nil
}

func Credentials(m Mapping) error {
	if m.Username != nil && (len(*m.Username) > 128 || strings.IndexFunc(*m.Username, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0) {
		return errors.New("invalid mapping username: at most 128 UTF-8 bytes without whitespace or control characters")
	}
	if m.Password != nil && (len(*m.Password) == 0 || len(*m.Password) > 1024 || strings.ContainsAny(*m.Password, "\r\n\x00")) {
		return errors.New("invalid mapping password: requires 1 to 1024 UTF-8 bytes")
	}
	return nil
}

func Path(config, path string) string {
	if path == "" {
		return ""
	}
	if !filepath.IsAbs(path) && config != "" {
		path = filepath.Join(filepath.Dir(config), path)
	}
	absolute, err := filepath.Abs(path)
	if err == nil {
		return absolute
	}
	return path
}

func Read(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open proxy mapping file; check path and permissions")
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > MaxBytes {
		return nil, errors.New("proxy mapping file must be a regular file of at most 1 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if err != nil || len(data) > MaxBytes {
		return nil, errors.New("cannot read proxy mapping file within 1 MiB limit")
	}
	return data, nil
}
