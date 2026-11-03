//go:build !linux && !darwin

package vpn

import "errors"

func ApplyNetwork(NetworkOptions) (func(), error) {
	return nil, errors.New("VPN network setup supports Linux and macOS")
}
