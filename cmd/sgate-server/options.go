package main

import (
	"errors"
	"flag"
	"sgate/internal/cli"
	"time"
)

// parseOptions 保留原有命令行和配置文件优先级，启动流程无需展开参数定义。
func parseOptions(args []string) (string, error) {
	adminEnabled := flag.Bool("admin-enabled", false, "enable management configured in the same service .cf")
	adminConfig := flag.String("admin-config", "", "separate Web management configuration (disabled when empty)")
	flag.String("listen", ":8081", "HTTP listen address")
	flag.String("target", "", "fixed backend TCP host:port (required)")
	flag.String("allowed-targets", "", "comma-separated host:port allowlist for client addr query (empty disables selection)")
	flag.String("path", "/tunnel", "WebSocket endpoint")
	flag.String("key-file", "", "shared key file (defaults to SGATE_KEY environment variable)")
	flag.String("password", "", "connection authentication password (required)")
	flag.String("tls-cert", "", "TLS certificate PEM file")
	flag.String("tls-key", "", "TLS private key PEM file")
	flag.Duration("session-ttl", 2*time.Minute, "retain disconnected sessions for this duration")
	flag.Int("max-sessions", 128, "maximum retained sessions")
	flag.Int("max-streams", 128, "maximum TCP streams per session")
	flag.Int("buffer-bytes", 4*1024*1024, "unacknowledged bytes per session and direction")
	flag.Duration("heartbeat", 20*time.Second, "WebSocket heartbeat interval")
	flag.Duration("heartbeat-timeout", 0, "receive idle timeout (0 uses three heartbeat intervals)")
	flag.Duration("noise-interval", 0, "random padding interval (0 disables sending)")
	flag.Duration("noise-interval-min", 0, "minimum random padding wait (requires noise-interval-max)")
	flag.Duration("noise-interval-max", 0, "maximum random padding wait (requires noise-interval-min)")
	flag.Int("noise-min-bytes", 1024, "minimum random padding payload bytes")
	flag.Int("noise-max-bytes", 10*1024, "maximum random padding payload bytes (up to 16384)")
	if err := cli.Parse(flag.CommandLine, args); err != nil {
		return "", err
	}
	if *adminEnabled && *adminConfig == "" {
		*adminConfig = flag.Lookup("config").Value.String()
		if *adminConfig == "" {
			return "", errors.New("admin-enabled requires a configuration file")
		}
	}

	return *adminConfig, nil
}
