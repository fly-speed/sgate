package main

import (
	"errors"
	"flag"
	"os"
	"time"

	"sgate/internal/cli"
)

// clientOptions 集中保存命令行/配置文件合并后的值，main 只负责选择运行方式。
type clientOptions struct {
	adminEnabled     bool
	adminConfig      string
	listen           string
	server           string
	proxies          string
	proxiesFile      string
	addr             string
	sendAddr         bool
	keyFile          string
	password         string
	username         string
	ca               string
	minRetry         time.Duration
	maxRetry         time.Duration
	offlineTimeout   time.Duration
	maxStreams       int
	buffer           int
	heartbeat        time.Duration
	heartbeatTimeout time.Duration
	noiseInterval    time.Duration
	noiseIntervalMin time.Duration
	noiseIntervalMax time.Duration
	noiseMin         int
	noiseMax         int
}

func parseClientOptions() (clientOptions, error) {
	var options clientOptions
	flag.BoolVar(
		&options.adminEnabled,
		"admin-enabled",
		false,
		"enable management configured in the same service .cf",
	)
	flag.StringVar(
		&options.adminConfig,
		"admin-config",
		"",
		"separate Web management configuration (disabled when empty)",
	)
	flag.StringVar(&options.listen, "listen", "127.0.0.1:9000", "local TCP listen address")
	flag.StringVar(&options.server, "server", "", "server WebSocket URL (required unless proxies is set)")
	flag.StringVar(&options.proxiesFile, "proxies-file", "", "external JSON mapping file; relative to the application config; created on startup if missing; overrides inline proxies")
	flag.StringVar(&options.proxies, "proxies", "", "JSON array of listen/server/addr mappings; empty uses single-proxy settings")
	flag.BoolVar(
		&options.sendAddr,
		"send-addr",
		true,
		"include backend addr in the request URL; false uses the server default",
	)
	flag.StringVar(&options.addr, "addr", "", "optional server-side backend host:port; overrides server URL addr when nonempty")
	flag.StringVar(&options.keyFile, "key-file", "", "shared key file (defaults to SGATE_KEY environment variable)")
	flag.StringVar(&options.username, "username", "", "tunnel account username; empty uses legacy single-password authentication")
	flag.StringVar(&options.password, "password", "", "connection authentication password (required)")
	flag.StringVar(&options.ca, "tls-ca", "", "additional trusted CA PEM file")
	flag.DurationVar(&options.minRetry, "reconnect-min", 250*time.Millisecond, "minimum reconnect interval")
	flag.DurationVar(&options.maxRetry, "reconnect-max", 10*time.Second, "maximum reconnect interval")
	flag.DurationVar(
		&options.offlineTimeout,
		"offline-timeout",
		2*time.Minute,
		"retain application sockets and replay buffer while offline",
	)
	flag.IntVar(&options.maxStreams, "max-streams", 128, "maximum simultaneous TCP streams")
	flag.IntVar(&options.buffer, "buffer-bytes", 4*1024*1024, "unacknowledged bytes per session and direction")
	flag.DurationVar(&options.heartbeat, "heartbeat", 20*time.Second, "WebSocket heartbeat interval")
	flag.DurationVar(
		&options.heartbeatTimeout,
		"heartbeat-timeout",
		0,
		"receive idle timeout (0 uses three heartbeat intervals)",
	)
	flag.DurationVar(&options.noiseInterval, "noise-interval", 0, "random padding interval (0 disables sending)")
	flag.DurationVar(
		&options.noiseIntervalMin,
		"noise-interval-min",
		0,
		"minimum random padding wait (requires noise-interval-max)",
	)
	flag.DurationVar(
		&options.noiseIntervalMax,
		"noise-interval-max",
		0,
		"maximum random padding wait (requires noise-interval-min)",
	)
	cli.ByteSizeVar(flag.CommandLine, &options.noiseMin, "noise-min-bytes", 1024, "minimum random padding bytes (supports K/M)")
	cli.ByteSizeVar(
		flag.CommandLine,
		&options.noiseMax,
		"noise-max-bytes",
		10*1024,
		"maximum random padding bytes per burst (up to 10M; supports K/M)",
	)
	cli.NoisePolicyFlags(flag.CommandLine)
	if err := cli.Parse(flag.CommandLine, os.Args[1:]); err != nil {
		return options, err
	}
	if options.adminEnabled && options.adminConfig == "" {
		options.adminConfig = flag.Lookup("config").Value.String()
		if options.adminConfig == "" {
			return options, errors.New("admin-enabled requires a configuration file")
		}
	}
	return options, nil
}
