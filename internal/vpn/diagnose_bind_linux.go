//go:build linux

package vpn

import (
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

func bindDiagnosticInterface(dialer *net.Dialer, name string) error {
	if _, err := net.InterfaceByName(name); err != nil {
		return err
	}
	dialer.Control = func(_, _ string, conn syscall.RawConn) error {
		var bindErr error
		if err := conn.Control(func(fd uintptr) {
			bindErr = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, name)
		}); err != nil {
			return err
		}
		return bindErr
	}
	return nil
}
