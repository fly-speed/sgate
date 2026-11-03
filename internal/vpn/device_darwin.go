//go:build darwin

package vpn

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func OpenTUN(name string) (*TUN, error) {
	unit := uint32(0)
	if name != "" && name != "utun" {
		if !strings.HasPrefix(name, "utun") {
			return nil, fmt.Errorf("macOS TUN name must be utun or utunN")
		}
		n, err := strconv.ParseUint(strings.TrimPrefix(name, "utun"), 10, 31)
		if err != nil {
			return nil, err
		}
		unit = uint32(n) + 1
	}
	fd, err := unix.Socket(unix.AF_SYSTEM, unix.SOCK_DGRAM, 2)
	if err != nil {
		return nil, err
	}
	info := unix.CtlInfo{}
	copy(info.Name[:], "com.apple.net.utun_control")
	if err = unix.IoctlCtlInfo(fd, &info); err != nil {
		unix.Close(fd)
		return nil, err
	}
	if err = unix.Connect(fd, &unix.SockaddrCtl{ID: info.Id, Unit: unit}); err != nil {
		unix.Close(fd)
		return nil, err
	}
	actual, err := unix.GetsockoptString(fd, 2, 2)
	if err != nil {
		unix.Close(fd)
		return nil, err
	}
	if err = unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		return nil, err
	}
	return &TUN{file: os.NewFile(uintptr(fd), "sgate-utun"), name: strings.TrimRight(actual, "\x00"), framed: true}, nil
}
