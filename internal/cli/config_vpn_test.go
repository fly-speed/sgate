package cli

import (
	"flag"
	"io"
	"strings"
	"testing"
)

func TestDisabledVPNConfigCannotBlockProxyStartup(t *testing.T) {
	for _, merged := range []bool{false, true} {
		for _, modeLast := range []bool{false, true} {
			body := "vpn-server = quic://127.0.0.1:8444/vpn\nvpn-client-ip = malformed-but-unused\nvpn-mtu = invalid-but-unused\nvpn-manage-network = invalid-but-unused\nlisten = 127.0.0.1:8081\n"
			if modeLast {
				body += "vpn-mode = disabled\n"
			} else {
				body = "vpn-mode = disabled\n" + body
			}
			if merged {
				body = "service server {\n" + body + "}\n"
			}
			fs := flag.NewFlagSet("server", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			fs.String("vpn-mode", "disabled", "")
			mtu := fs.Int("vpn-mtu", 1280, "")
			manage := fs.Bool("vpn-manage-network", true, "")
			listen := fs.String("listen", "", "")
			if e := Parse(fs, []string{"-config", configFile(t, body)}); e != nil {
				t.Fatal(merged, modeLast, e)
			}
			if *mtu != 1280 || !*manage || *listen != "127.0.0.1:8081" {
				t.Fatal("inactive VPN affected runtime or proxy settings")
			}
		}
	}
}
func TestVPNConfigModeUsesCLIOverrideAndKeepsEnabledValidation(t *testing.T) {
	for _, merged := range []bool{false, true} {
		for _, tc := range []struct {
			fileMode, override string
			wantError          bool
		}{
			{"disabled", "", false}, {"", "", false}, {"both", "", true}, {"only", "", true}, {"disabled", "both", true}, {"both", "disabled", false},
		} {
			body := "vpn-server = quic://127.0.0.1:8444/vpn\n"
			if tc.fileMode != "" {
				body += "vpn-mode = " + tc.fileMode + "\n"
			}
			if merged {
				body = "service server {\n" + body + "}\n"
			}
			fs := flag.NewFlagSet("server", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			fs.String("vpn-mode", "disabled", "")
			args := []string{"-config", configFile(t, body)}
			if tc.override != "" {
				args = append(args, "-vpn-mode", tc.override)
			}
			e := Parse(fs, args)
			if (e != nil) != tc.wantError {
				t.Fatal(merged, tc, e)
			}
		}
	}
}
func TestDisabledVPNDoesNotRelaxOtherConfigurationErrors(t *testing.T) {
	for _, merged := range []bool{false, true} {
		for _, body := range []string{
			"vpn-mode = disabled\nnon-vpn-option = value\n",
			"vpn-mode = disabled\nvpn-mtu = 1200\nvpn-mtu = 1280\n",
			"vpn-mode = disabled\nvpn-key-file = 'unterminated\n",
		} {
			if merged {
				body = "service server {\n" + body + "}\n"
			}
			fs := flag.NewFlagSet("server", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			fs.String("vpn-mode", "disabled", "")
			fs.Int("vpn-mtu", 1280, "")
			if e := Parse(fs, []string{"-config", configFile(t, body)}); e == nil {
				t.Fatal("invalid config accepted", merged)
			}
		}
	}
	// A parser without VPN support must not silently accept optional sections.
	fs := flag.NewFlagSet("other", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if e := Parse(fs, []string{"-config", configFile(t, "vpn-server = unused\n")}); e == nil || !strings.Contains(e.Error(), "unknown") {
		t.Fatal(e)
	}
}
