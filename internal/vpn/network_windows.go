//go:build windows

package vpn

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func ApplyNetwork(o NetworkOptions) (func(), error) {
	return applyWindowsNetwork(o, runWindowsNetworkCommand)
}

func runWindowsNetworkCommand(_ string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	program := filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	output, err := exec.CommandContext(ctx, program, args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("Windows VPN network setup: %w: %.1000s", err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}
