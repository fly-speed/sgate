package tunnel

import (
	"errors"

	"golang.org/x/sys/windows"
)

func windowsDialResetReason(err error) byte {
	switch {
	case errors.Is(err, windows.WSAECONNREFUSED):
		return resetRefused
	case errors.Is(err, windows.WSAENETUNREACH), errors.Is(err, windows.WSAEHOSTUNREACH):
		return resetUnreachable
	default:
		return resetDialFailed
	}
}

func windowsPeerClosed(err error) bool {
	return errors.Is(err, windows.WSAECONNRESET) || errors.Is(err, windows.WSAENOTCONN)
}
