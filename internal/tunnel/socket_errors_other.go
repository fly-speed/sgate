//go:build !windows

package tunnel

func windowsDialResetReason(error) byte { return resetDialFailed }
func windowsPeerClosed(error) bool      { return false }
