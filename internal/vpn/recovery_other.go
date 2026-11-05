//go:build !darwin && !linux && !windows

package vpn

import (
	"errors"
	"os"
)

var errRecoveryUnsupported = errors.New("VPN network recovery is unsupported on this platform")

func lockRecoveryFile(*os.File) error              { return errRecoveryUnsupported }
func recoverClientDNS(clientRecoveryState) error   { return errRecoveryUnsupported }
func recoverClientRoute(clientRecoveryState) error { return errRecoveryUnsupported }

func trustedRecoveryFile(*os.File, os.FileInfo) bool { return false }
