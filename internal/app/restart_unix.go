//go:build unix

package app

import (
	"os/exec"
	"syscall"
)

func detachRestart(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return nil
}
