package cli

import "flag"

func ResourceFlags(fs *flag.FlagSet) {
	fs.Int("quic-initial-packet-bytes", 1200, "initial QUIC UDP payload size (1200..1452); path MTU discovery remains enabled")
	for name, help := range map[string]string{
		"quic-stream-window":     "maximum QUIC receive window per stream",
		"quic-connection-window": "maximum QUIC receive window per connection",
		"quic-stream-buffer":     "native business queue limit per stream and direction",
		"quic-replay-bytes":      "native session replay queue budget",
		"quic-receive-bytes":     "native session receive queue budget",
	} {
		ByteSizeVar(fs, new(int), name, 0, help+" (0 derives from buffer-bytes; supports K/M)")
	}
	ByteSizeVar(fs, new(int), "memory-budget-bytes", 0, "process managed-buffer reservation limit (0 disables; excludes runtime/kernel overhead)")
}
