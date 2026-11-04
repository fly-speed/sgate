// Package clientcontrol implements the private inherited control channel used
// by the desktop launcher. Standalone clients never enable this channel.
package clientcontrol

import (
	"context"
	"io"
)

const Environment = "SGATE_CONTROL_STDIN"

// Watch cancels on a stop byte or EOF (including launcher crashes). It never
// kills the process: the ordinary application shutdown owns VPN cleanup.
func Watch(ctx context.Context, cancel context.CancelFunc, input io.ReadCloser) func() {
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { input.Close() })
	go func() { defer close(done); var b [1]byte; input.Read(b[:]); cancel() }()
	return func() { stop(); input.Close(); <-done }
}
