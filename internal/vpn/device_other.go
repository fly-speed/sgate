//go:build !linux && !darwin && !windows

package vpn

import "errors"

func OpenTUN(string) (*TUN, error) {
	return nil, errors.New("sgate VPN currently supports Linux and macOS")
}
