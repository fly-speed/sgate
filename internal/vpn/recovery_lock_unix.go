//go:build darwin || linux

package vpn

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func lockRecoveryFile(f *os.File) error {
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fmt.Errorf("VPN 网络设置正由其他进程使用：%w", err)
	}
	return nil
}

func trustedRecoveryFile(_ *os.File, info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid()) && info.Mode().Perm()&0077 == 0
}
