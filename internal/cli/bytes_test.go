package cli

import (
	"flag"
	"testing"
)

func TestByteSizeParsingAndClone(t *testing.T) {
	for text, want := range map[string]int{"0": 0, "10240": 10240, "1K": 1024, "10M": 10485760, "10MiB": 10485760, "3kb": 3072} {
		got, err := ParseByteSize(text)
		if err != nil || got != want {
			t.Fatalf("%q: %d, %v", text, got, err)
		}
	}
	for _, text := range []string{"-1", "1.5M", "10G", "M", "18446744073709551615M"} {
		if _, err := ParseByteSize(text); err == nil {
			t.Fatalf("accepted %q", text)
		}
	}
	fs := flag.NewFlagSet("sizes", flag.ContinueOnError)
	ByteSizeVar(fs, new(int), "noise-max-bytes", 10240, "")
	path := configFile(t, "noise-max-bytes = 10M\n")
	if err := Parse(fs, []string{"-config", path}); err != nil {
		t.Fatal(err)
	}
	copy := CloneFlags(fs)
	if err := Parse(copy, []string{"-config", path, "-noise-max-bytes", "2M"}); err != nil {
		t.Fatal(err)
	}
	if fs.Lookup("noise-max-bytes").Value.String() != "10485760" || copy.Lookup("noise-max-bytes").Value.String() != "2097152" {
		t.Fatal("size flag type or precedence was lost during clone")
	}
}
