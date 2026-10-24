package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"sgate/internal/aclservice"
	"sgate/internal/app"
	"sgate/internal/cli"
	"sgate/internal/tunnel"
)

func main() {
	adminEnabled := flag.Bool("admin-enabled", false, "enable management configured in the same service .cf")
	adminConfig := flag.String("admin-config", "", "separate Web management configuration (disabled when empty)")
	listen := flag.String("listen", "127.0.0.1:9000", "local TCP listen address")
	server := flag.String("server", "", "server WebSocket URL (required)")
	keyFile := flag.String("key-file", "", "shared key file (defaults to SGATE_KEY environment variable)")
	password := flag.String("password", "", "connection authentication password (required)")
	ca := flag.String("tls-ca", "", "additional trusted CA PEM file")
	minRetry := flag.Duration("reconnect-min", 250*time.Millisecond, "minimum reconnect interval")
	maxRetry := flag.Duration("reconnect-max", 10*time.Second, "maximum reconnect interval")
	offlineTimeout := flag.Duration("offline-timeout", 2*time.Minute, "retain application sockets and replay buffer while offline")
	maxStreams := flag.Int("max-streams", 128, "maximum simultaneous TCP streams")
	buffer := flag.Int("buffer-bytes", 4*1024*1024, "unacknowledged bytes per session and direction")
	heartbeat := flag.Duration("heartbeat", 20*time.Second, "WebSocket heartbeat interval")
	heartbeatTimeout := flag.Duration("heartbeat-timeout", 0, "receive idle timeout (0 uses three heartbeat intervals)")
	noiseInterval := flag.Duration("noise-interval", 0, "random padding interval (0 disables sending)")
	noiseIntervalMin := flag.Duration("noise-interval-min", 0, "minimum random padding wait (requires noise-interval-max)")
	noiseIntervalMax := flag.Duration("noise-interval-max", 0, "maximum random padding wait (requires noise-interval-min)")
	noiseMin := flag.Int("noise-min-bytes", 1024, "minimum random padding payload bytes")
	noiseMax := flag.Int("noise-max-bytes", 10*1024, "maximum random padding payload bytes (up to 16384)")
	if err := cli.Parse(flag.CommandLine, os.Args[1:]); err != nil {
		log.Fatal(err)
	}
	if *adminEnabled && *adminConfig == "" {
		*adminConfig = flag.Lookup("config").Value.String()
		if *adminConfig == "" {
			log.Fatal("admin-enabled requires a configuration file")
		}
	}
	if aclservice.Enabled() {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := app.RunACL(ctx, "client", flag.CommandLine, os.Args[1:], *adminConfig, nil); err != nil {
			log.Fatal(err)
		}
		return
	}
	if *adminConfig != "" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := app.Run(ctx, "client", flag.CommandLine, os.Args[1:], *adminConfig); err != nil {
			log.Fatal(err)
		}
		return
	}
	secret, err := cli.LoadKey(*keyFile)
	if err != nil {
		log.Fatal(err)
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if *ca != "" {
		pem, err := os.ReadFile(*ca)
		if err != nil {
			log.Fatal(err)
		}
		roots, err := x509.SystemCertPool()
		if err != nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(pem) {
			log.Fatal("no CA certificates found")
		}
		tlsConfig.RootCAs = roots
	}
	client, err := tunnel.NewClient(tunnel.ClientConfig{Key: secret, Password: *password, URL: *server, TLSConfig: tlsConfig, ReconnectMin: *minRetry, ReconnectMax: *maxRetry, OfflineTimeout: *offlineTimeout, Options: tunnel.Options{BufferBytes: *buffer, MaxStreams: *maxStreams, Heartbeat: *heartbeat, HeartbeatTimeout: *heartbeatTimeout, NoiseInterval: *noiseInterval, NoiseIntervalMin: *noiseIntervalMin, NoiseIntervalMax: *noiseIntervalMax, NoiseMinBytes: *noiseMin, NoiseMaxBytes: *noiseMax}})
	if err != nil {
		log.Fatal(err)
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("sgate client listening on %s", listener.Addr())
	if err = client.Run(ctx, listener); err != nil {
		log.Fatal(err)
	}
}
