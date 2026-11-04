//go:build !linux

package vpn

import (
	"errors"
	"net"
)

func setTCPNotSentLowat(net.Conn, int) error {
	return errors.New("TCP_NOTSENT_LOWAT is enabled by sgate on Linux only")
}
