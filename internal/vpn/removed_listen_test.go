package vpn

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"testing"

	"sgate/internal/cli"
)

func TestRemovedVPNListenIsRejected(t *testing.T) {
	for _, mode := range []string{"disabled", "both"} {
		for _, merged := range []bool{false, true} {
			fs := flag.NewFlagSet("server", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			RegisterFlags(fs, "server")
			content := "vpn-mode = " + mode + "\nvpn-listen = 127.0.0.1:8444\n"
			if merged {
				content = "service server {\n" + content + "}\n"
			}
			path := filepath.Join(t.TempDir(), "server.cf")
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			if err := cli.Parse(fs, []string{"-config", path}); err == nil {
				t.Fatal("removed vpn-listen accepted", mode, merged)
			}
		}
	}
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	RegisterFlags(fs, "server")
	if err := fs.Parse([]string{"-vpn-listen", ":8444"}); err == nil {
		t.Fatal("removed command-line option accepted")
	}
}
