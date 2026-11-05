package vpn

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func lockRecoveryFile(f *os.File) error {
	var overlap windows.Overlapped
	if err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlap); err != nil {
		return fmt.Errorf("VPN 网络设置正由其他进程使用：%w", err)
	}
	return nil
}

func trustedRecoveryFile(f *os.File, _ os.FileInfo) bool {
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return false
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil {
		return false
	}
	tokenUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	return err == nil && (owner.Equals(tokenUser.User.Sid) ||
		owner.IsWellKnown(windows.WinBuiltinAdministratorsSid) || owner.IsWellKnown(windows.WinLocalSystemSid))
}
