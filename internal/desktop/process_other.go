//go:build !windows

package desktop

import "os/exec"

func configureProcess(*exec.Cmd) {}
