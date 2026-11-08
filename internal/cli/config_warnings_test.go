package cli

import (
	"bytes"
	"flag"
	"io"
	"log"
	"strings"
	"testing"
	"time"
)

func warningClientFlags() *flag.FlagSet {
	fs := flag.NewFlagSet("client", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.String("proxies", "", "")
	fs.String("proxies-file", "", "")
	fs.Duration("heartbeat", time.Second, "")
	return fs
}

func captureConfigWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var out bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&out)
	t.Cleanup(func() { log.SetOutput(previous) })
	return &out
}

func TestClientUnknownConfigWarningsPreserveKnownSettings(t *testing.T) {
	for _, merged := range []bool{false, true} {
		name := "plain"
		if merged { name = "service" }
		t.Run(name, func(t *testing.T) {
			out := captureConfigWarnings(t)
			content := "listen = private-listener\nserver = private-server-value\nfuture-setting = private-future-value\nheartbeat = 2s\n"
			if merged { content = "service client {\n" + content + "}\n" }
			path := configFile(t, content)
			fs := warningClientFlags()
			if err := Parse(fs, []string{"-config", path, "-heartbeat", "3s"}); err != nil { t.Fatal(err) }
			if fs.Lookup("heartbeat").Value.String() != "3s" { t.Fatal("CLI precedence changed") }
			warnings := out.String()
			for _, key := range []string{"listen", "server", "future-setting"} {
				if !strings.Contains(warnings, "unknown option \""+key+"\" ignored") { t.Fatal("missing warning", warnings) }
			}
			for _, value := range []string{"private-listener", "private-server-value", "private-future-value"} {
				if strings.Contains(warnings, value) { t.Fatal("configuration value leaked") }
			}
			line := "line 1:"
			if merged { line = "line 2:" }
			if !strings.Contains(warnings, line) { t.Fatal("source line missing", warnings) }
			out.Reset()
			// Startup initializes a different log sink after parsing the file.
			LogClientConfigWarnings(fs)
			if !strings.Contains(out.String(), "unknown option \"server\" ignored") { t.Fatal("startup warnings not replayed into service log") }
			out.Reset()
			if err := Parse(CloneFlags(fs), []string{"-config", path}); err != nil { t.Fatal("reload rejected unknown fields", err) }
			if !strings.Contains(out.String(), "unknown option \"listen\" ignored") { t.Fatal("reload warning missing") }
		})
	}
}

func TestClientUnknownOptionsDoNotRelaxOtherErrors(t *testing.T) {
	for _, content := range []string{
		"heartbeat = not-a-duration\n",
		"heartbeat = 1s\nheartbeat = 2s\n",
		"future-setting = 'unterminated\n",
		"config = nested.conf\n",
		"invalid syntax\n",
	} {
		for _, merged := range []bool{false,true} {
			text := content
			if merged { text = "service client {\n" + text + "}\n" }
			if err := Parse(warningClientFlags(), []string{"-config",configFile(t,text)}); err == nil { t.Fatal("malformed config accepted", content) }
		}
	}
	if err := Parse(warningClientFlags(), []string{"-server", "private-value"}); err == nil { t.Fatal("unknown command-line option accepted") }
	strict := flag.NewFlagSet("server", flag.ContinueOnError)
	strict.SetOutput(io.Discard)
	if err := Parse(strict, []string{"-config",configFile(t,"server = private-value\n")}); err == nil { t.Fatal("server config became permissive") }
}

func TestClientManagementUnknownOptionsWarn(t *testing.T) {
	for _, merged := range []bool{false,true} {
		out := captureConfigWarnings(t)
		content := "listen = 127.0.0.1:9081\nfuture-option = private-value\n"
		if merged { content = "service client {\nadmin_listen = 127.0.0.1:9081\nadmin_future_option = private-value\n}\n" }
		fs := flag.NewFlagSet("management",flag.ContinueOnError)
		listen := fs.String("listen","","")
		if err := ParseClientSection(fs,configFile(t,content),"admin_"); err != nil { t.Fatal(err) }
		if *listen != "127.0.0.1:9081" || !strings.Contains(out.String(), "ignored") || strings.Contains(out.String(),"private-value") { t.Fatal("management warning or setting incorrect", out.String()) }
	}
}
