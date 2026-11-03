//go:build linux

package vpn

import (
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

func OpenTUN(name string) (*TUN, error) {
	if name == "" {
		name = "sgate0"
	}
	if _, err := net.InterfaceByName(name); err == nil {
		return nil, fmt.Errorf("interface %s already exists; refusing to modify it", name)
	}
	req, err := unix.NewIfreq(name)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	req.SetUint16(unix.IFF_TUN | unix.IFF_NO_PI)
	if err = unix.IoctlIfreq(fd, unix.TUNSETIFF, req); err != nil {
		unix.Close(fd)
		return nil, err
	}
	if err = unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		return nil, err
	}
	return &TUN{file: os.NewFile(uintptr(fd), "sgate-tun"), name: req.Name()}, nil
}
