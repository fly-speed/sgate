//go:build windows

package vpn

import (
	"golang.org/x/sys/windows"
	"net"
	"syscall"
)

func bindDiagnosticInterface(dialer *net.Dialer, name string) error {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return err
	}
	dialer.Control = func(_, _ string, c syscall.RawConn) error {
		var bindErr error
		// IP_UNICAST_IF takes the interface index in network byte order.
		i := uint32(iface.Index)
		networkOrder := (i&255)<<24 | (i&0xff00)<<8 | (i&0xff0000)>>8 | i>>24
		if err := c.Control(func(fd uintptr) {
			bindErr = windows.SetsockoptInt(windows.Handle(fd), windows.IPPROTO_IP, 31 /* IP_UNICAST_IF */, int(networkOrder))
		}); err != nil {
			return err
		}
		return bindErr
	}
	return nil
}
