//go:build darwin || linux

package vpn

import (
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

func udpListenConfig() net.ListenConfig {
	return net.ListenConfig{Control: func(_, _ string, conn syscall.RawConn) error {
		return conn.Control(func(fd uintptr) {
			// Best effort and local to this listener. The kernel can clamp the
			// requested size to the host limit without failing startup.
			_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF, udpReceiveBuffer)
		})
	}}
}
