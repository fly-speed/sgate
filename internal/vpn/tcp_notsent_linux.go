package vpn

import (
	"crypto/tls"
	"net"

	"golang.org/x/sys/unix"
)

func setTCPNotSentLowat(conn net.Conn, bytes int) error {
	if tlsConn, ok := conn.(*tls.Conn); ok {
		conn = tlsConn.NetConn()
	}
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		return unix.ENOTSOCK
	}
	raw, err := tcp.SyscallConn()
	if err != nil {
		return err
	}
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		socketErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_NOTSENT_LOWAT, bytes)
	}); err != nil {
		return err
	}
	return socketErr
}
