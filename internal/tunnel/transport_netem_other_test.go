//go:build !linux

package tunnel

import "testing"

func configureNetem(t *testing.T, _, _ string) {
	t.Helper()
	t.Fatal("Linux netem requires Linux and an isolated network namespace")
}
