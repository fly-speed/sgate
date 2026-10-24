package cli

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCloneFlagsPreservesFileAndExplicitPrecedence(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String("password", "", "secret")
	fs.Int("count", 7, "")
	fs.Bool("enabled", true, "")
	fs.Duration("delay", time.Second, "")
	path := filepath.Join(t.TempDir(), "app.conf")
	if e := os.WriteFile(path, []byte("password = file-password\ncount = 19\nenabled = false\ndelay = 3s\n"), 0600); e != nil {
		t.Fatal(e)
	}
	for _, override := range []bool{false, true} {
		copy := CloneFlags(fs)
		args := []string{"-config", path}
		if override {
			args = append(args, "-count", "7")
		}
		if e := Parse(copy, args); e != nil {
			t.Fatal(e)
		}
		want := "19"
		if override {
			want = "7"
		}
		if copy.Lookup("count").Value.String() != want || copy.Lookup("password").Value.String() != "file-password" || copy.Lookup("enabled").Value.String() != "false" || copy.Lookup("delay").Value.String() != "3s" {
			t.Fatal("file/CLI precedence lost")
		}
		if fs.Lookup("password").Value.String() != "" {
			t.Fatal("clone mutated original values")
		}
	}
}
