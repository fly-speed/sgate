package app

import (
	"reflect"
	"testing"
)

func TestAllowedTargetsConfigApplyAndCLIOverride(t *testing.T) {
	for _, override := range []bool{false, true} {
		name := "file"
		var args []string
		if override {
			name = "cli"
			args = []string{"-allowed-targets", "localhost:8088"}
		}
		t.Run(name, func(t *testing.T) {
			c := newController(t, "server", "target = 127.0.0.1:9001\nallowed-targets = 127.0.0.1:8088, example.com:443\n", args...)
			want := []string{"127.0.0.1:8088", "example.com:443"}
			if override {
				want = []string{"localhost:8088"}
			}
			if !reflect.DeepEqual(c.current.server.AllowedTargets, want) {
				t.Fatal(c.current.server.AllowedTargets)
			}
			setConfig(t, c, "allowed-targets", "127.0.0.1:8089")
			if err := c.apply(); err != nil {
				t.Fatal(err)
			}
			if !override {
				want = []string{"127.0.0.1:8089"}
			}
			if !reflect.DeepEqual(c.current.server.AllowedTargets, want) {
				t.Fatal("allowlist apply lost CLI priority")
			}
			if !override {
				worker := c.worker
				setConfig(t, c, "allowed-targets", "127.0.0.1:0")
				if err := c.apply(); err == nil || c.worker != worker {
					t.Fatal("invalid allowlist replaced a running proxy")
				}
			}
		})
	}
}
