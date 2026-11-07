package cli

import (
	"flag"
	"io"
	"testing"
)

func TestServerMasterServiceConfiguration(t *testing.T) {
	for _, content := range []string{"master_service = [::1]|1080, 127.0.0.1|8081\n", "service server {\nmaster_service = [::1]|1080, 127.0.0.1|8081\n}\n"} {
		fs := flag.NewFlagSet("server", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fs.String("master_service", "127.0.0.1|8081", "")
		if err := Parse(fs, []string{"-config", configFile(t, content)}); err != nil {
			t.Fatal(err)
		}
		if got := fs.Lookup("master_service").Value.String(); got != "[::1]|1080, 127.0.0.1|8081" {
			t.Fatal(got)
		}
	}
}

func TestServerRejectsRemovedListenOption(t *testing.T) {
	for _, content := range []string{"listen = :8081\n", "service server {\nlisten = :8081\n}\n", ""} {
		fs := flag.NewFlagSet("server", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fs.String("master_service", "127.0.0.1|8081", "")
		args := []string{"-listen", ":8081"}
		if content != "" {
			args = []string{"-config", configFile(t, content)}
		}
		if err := Parse(fs, args); err == nil {
			t.Fatal("removed listen option accepted", content)
		}
	}
}
