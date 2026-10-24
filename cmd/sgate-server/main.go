package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"sgate/internal/aclgin"
	"sgate/internal/aclservice"
	"sgate/internal/app"
	"sgate/internal/cli"
	"sgate/internal/tunnel"
)

func main() {
	adminConfig := flag.String("admin-config", "", "separate Web management configuration (disabled when empty)")
	listen := flag.String("listen", ":8080", "HTTP listen address")
	target := flag.String("target", "", "fixed backend TCP host:port (required)")
	path := flag.String("path", "/tunnel", "WebSocket endpoint")
	keyFile := flag.String("key-file", "", "shared key file (defaults to SGATE_KEY environment variable)")
	password := flag.String("password", "", "connection authentication password (required)")
	cert := flag.String("tls-cert", "", "TLS certificate PEM file")
	key := flag.String("tls-key", "", "TLS private key PEM file")
	ttl := flag.Duration("session-ttl", 2*time.Minute, "retain disconnected sessions for this duration")
	maxSessions := flag.Int("max-sessions", 128, "maximum retained sessions")
	maxStreams := flag.Int("max-streams", 128, "maximum TCP streams per session")
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
	if aclservice.Enabled() {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := app.RunACL(ctx, "server", flag.CommandLine, os.Args[1:], *adminConfig, aclgin.HTTPHandler); err != nil {
			log.Fatal(err)
		}
		return
	}
	if *adminConfig != "" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := app.Run(ctx, "server", flag.CommandLine, os.Args[1:], *adminConfig); err != nil {
			log.Fatal(err)
		}
		return
	}
	secret, err := cli.LoadKey(*keyFile)
	if err != nil {
		log.Fatal(err)
	}
	if (*cert == "") != (*key == "") {
		log.Fatal("both -tls-cert and -tls-key are required for TLS")
	}
	proxy, err := tunnel.NewServer(tunnel.ServerConfig{Key: secret, Password: *password, Target: *target, Path: *path, SessionTTL: *ttl, MaxSessions: *maxSessions, Options: tunnel.Options{BufferBytes: *buffer, MaxStreams: *maxStreams, Heartbeat: *heartbeat, HeartbeatTimeout: *heartbeatTimeout, NoiseInterval: *noiseInterval, NoiseIntervalMin: *noiseIntervalMin, NoiseIntervalMax: *noiseIntervalMax, NoiseMinBytes: *noiseMin, NoiseMaxBytes: *noiseMax}})
	if err != nil {
		log.Fatal(err)
	}
	defer proxy.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	server := &http.Server{Addr: *listen, Handler: proxy, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024}
	go func() {
		<-ctx.Done()
		proxy.Close()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	log.Printf("sgate server listening on %s; target %s", *listen, *target)
	if *cert != "" {
		err = server.ListenAndServeTLS(*cert, *key)
	} else {
		err = server.ListenAndServe()
	}
	if err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
