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

	"sgate/internal/cli"
	"sgate/internal/tunnel"
)

func main() {
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
	flag.Parse()
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
	client, err := tunnel.NewClient(tunnel.ClientConfig{Key: secret, Password: *password, URL: *server, TLSConfig: tlsConfig, ReconnectMin: *minRetry, ReconnectMax: *maxRetry, OfflineTimeout: *offlineTimeout, Options: tunnel.Options{BufferBytes: *buffer, MaxStreams: *maxStreams, Heartbeat: *heartbeat, HeartbeatTimeout: *heartbeatTimeout}})
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
