package cli

import (
	"bytes"
	"flag"
	"io"
	"testing"
	"time"

	"sgate/internal/tunnel"
)

func TestConfigNoiseOptionPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name             string
		args             []string
		min, max         time.Duration
		sizeMin, sizeMax int
		fixed            time.Duration
		invalid          bool
	}{
		{name: "file", min: 20 * time.Millisecond, max: 60 * time.Millisecond, sizeMin: 1024, sizeMax: 4096},
		{name: "override", args: []string{"-noise-interval-min", "5ms", "-noise-interval-max", "15ms", "-noise-min-bytes", "128", "-noise-max-bytes", "512"}, min: 5 * time.Millisecond, max: 15 * time.Millisecond, sizeMin: 128, sizeMax: 512},
		{name: "disable", args: []string{"-noise-interval-min=0", "-noise-interval-max=0"}, sizeMin: 1024, sizeMax: 4096},
		{name: "fixed", args: []string{"-noise-interval-min=0", "-noise-interval-max=0", "-noise-interval=10ms"}, fixed: 10 * time.Millisecond, sizeMin: 1024, sizeMax: 4096},
		{name: "one-bound-only", args: []string{"-noise-interval-min=0"}, max: 60 * time.Millisecond, sizeMin: 1024, sizeMax: 4096, invalid: true},
		{name: "fixed-range-conflict", args: []string{"-noise-interval=10ms"}, fixed: 10 * time.Millisecond, min: 20 * time.Millisecond, max: 60 * time.Millisecond, sizeMin: 1024, sizeMax: 4096, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("noise", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			fixed := fs.Duration("noise-interval", 0, "")
			min := fs.Duration("noise-interval-min", 0, "")
			max := fs.Duration("noise-interval-max", 0, "")
			sizeMin := fs.Int("noise-min-bytes", 1024, "")
			sizeMax := fs.Int("noise-max-bytes", 10240, "")
			path := configFile(t, "noise-interval = 0\nnoise-interval-min = 20ms\nnoise-interval-max = 60ms\nnoise-min-bytes = 1024\nnoise-max-bytes = 4096\n")
			args := append([]string{"-config", path}, tc.args...)
			if err := Parse(fs, args); err != nil {
				t.Fatal(err)
			}
			if *min != tc.min || *max != tc.max || *fixed != tc.fixed || *sizeMin != tc.sizeMin || *sizeMax != tc.sizeMax {
				t.Fatal("effective noise options do not match precedence")
			}
			server, err := tunnel.NewServer(tunnel.ServerConfig{Key: bytes.Repeat([]byte{27}, 32), Password: "test-password", Target: "127.0.0.1:1", Options: tunnel.Options{NoiseInterval: *fixed, NoiseIntervalMin: *min, NoiseIntervalMax: *max, NoiseMinBytes: *sizeMin, NoiseMaxBytes: *sizeMax}})
			if err == nil {
				server.Close()
			}
			if (err != nil) != tc.invalid {
				t.Fatalf("range validation: invalid=%t err=%v", tc.invalid, err)
			}
		})
	}
}
