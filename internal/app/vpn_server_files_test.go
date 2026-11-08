package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestServerVPNFilePathsResolveDormantSettingsAndOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.conf")
	content := "vpn-mode = disabled\nvpn-tls-cert = vpn-cert.pem\ntls-key = proxy-key.pem\nkey-file = shared.key\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	c := &controller{
		role: "server", path: path, original: appFlags("server"),
		args: []string{"-config", path, "-vpn-tls-key=override-key.pem"}, overrides: []string{"vpn-tls-key"},
	}
	paths, err := c.serverVPNFilePaths()
	if err != nil {
		t.Fatal(err)
	}
	for field, name := range map[string]string{
		"vpn-tls-cert": "vpn-cert.pem", "vpn-tls-key": "override-key.pem", "vpn-key-file": "shared.key",
	} {
		want, err := filepath.Abs(name)
		if err != nil || paths[field] != want {
			t.Fatalf("%s: got %q, want %q (%v)", field, paths[field], want, err)
		}
	}
}

func TestServerVPNKeyPathTreatsQuotedEmptyValuesAsUnset(t *testing.T) {
	for _, tc := range []struct {
		name, keyFile, vpnKeyFile, override, want string
	}{
		{"quoted empty", "shared.key", "''", "", "shared.key"},
		{"saved literal quotes", "shared.key", "\"''\"", "", "shared.key"},
		{"launcher literal quotes", "shared.key", "vpn.key", "-vpn-key-file=''", "shared.key"},
		{"no file", "\"''\"", "\"''\"", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "server.cf")
			content := "service server {\n vpn-mode = disabled\n key-file = " + tc.keyFile +
				"\n vpn-key-file = " + tc.vpnKeyFile + "\n}\n"
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			c := &controller{role: "server", path: path, original: appFlags("server"), args: []string{"-config", path}}
			if tc.override != "" {
				c.args = append(c.args, tc.override)
				c.overrides = []string{"vpn-key-file"}
			}
			paths, err := c.serverVPNFilePaths()
			if err != nil {
				t.Fatal(err)
			}
			want := tc.want
			if want != "" {
				want, err = filepath.Abs(want)
				if err != nil {
					t.Fatal(err)
				}
			}
			if got, ok := paths["vpn-key-file"]; !ok || got != want {
				t.Fatalf("VPN key path = %q, present = %t, want %q", got, ok, want)
			}
		})
	}
}

func TestServerVPNFilePathsIncludeUnconfiguredCertificateFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.conf")
	if err := os.WriteFile(path, []byte("vpn-mode = disabled\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c := &controller{role: "server", path: path, original: appFlags("server"), args: []string{"-config", path}}
	paths, err := c.serverVPNFilePaths()
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"vpn-tls-cert", "vpn-tls-key"} {
		if path, ok := paths[field]; !ok || path != "" {
			t.Fatalf("%s: path=%q, present=%t", field, path, ok)
		}
	}
}
