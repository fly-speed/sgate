//go:build !unix

package app

import (
	"errors"
	"os/exec"
)

func detachRestart(*exec.Cmd) error {
	return errors.New("当前平台不支持 acl_master 服务重启")
}
