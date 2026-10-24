package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"

	"sgate/internal/cli"
	"sgate/internal/tunnel"
)

// 独立命令行模式仍使用同一套参数和验证规则；监听与退出等待留在 main。
func newStandaloneClient(options clientOptions) (*tunnel.Client, error) {
	secret, err := cli.LoadKey(options.keyFile)
	if err != nil {
		return nil, err
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if options.ca != "" {
		pem, err := os.ReadFile(options.ca)
		if err != nil {
			return nil, err
		}
		roots, err := x509.SystemCertPool()
		if err != nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, errors.New("no CA certificates found")
		}
		tlsConfig.RootCAs = roots
	}
	client, err := tunnel.NewClient(tunnel.ClientConfig{
		Key:            secret,
		Password:       options.password,
		URL:            options.server,
		Addr:           options.addr,
		OmitAddr:       !options.sendAddr,
		TLSConfig:      tlsConfig,
		ReconnectMin:   options.minRetry,
		ReconnectMax:   options.maxRetry,
		OfflineTimeout: options.offlineTimeout,
		Options: tunnel.Options{
			BufferBytes:      options.buffer,
			MaxStreams:       options.maxStreams,
			Heartbeat:        options.heartbeat,
			HeartbeatTimeout: options.heartbeatTimeout,
			NoiseInterval:    options.noiseInterval,
			NoiseIntervalMin: options.noiseIntervalMin,
			NoiseIntervalMax: options.noiseIntervalMax,
			NoiseMinBytes:    options.noiseMin,
			NoiseMaxBytes:    options.noiseMax,
		},
	})
	return client, err
}
