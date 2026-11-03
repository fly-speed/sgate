//go:build darwin

package vpn

import (
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

func bindDiagnosticInterface(dialer *net.Dialer, name string) error {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return err
	}
	dialer.Control = func(_, _ string, conn syscall.RawConn) error {
		var bindErr error
		if err := conn.Control(func(fd uintptr) {
			bindErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_BOUND_IF, iface.Index)
		}); err != nil {
			return err
		}
		return bindErr
	}
	return nil
}
