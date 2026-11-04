//go:build !darwin && !linux && !windows

package vpn

import (
	"errors"
	"net"
)

func bindDiagnosticInterface(*net.Dialer, string) error {
	return errors.New("isolated VPN diagnosis requires macOS or Linux")
}
