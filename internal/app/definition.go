package app

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"sgate/internal/cli"
	"sgate/internal/tunnel"
)

// configValues 只读取已经完成类型校验和命令行覆盖合并的参数。
// 配置文件与命令行的优先级仍由 cli.Parse 统一决定。
type configValues struct{ flags *flag.FlagSet }

func (v configValues) text(name string) string { return v.flags.Lookup(name).Value.String() }
func (v configValues) boolean(name string) bool {
	value, _ := strconv.ParseBool(v.text(name))
	return value
}
func (v configValues) integer(name string) int {
	value, _ := strconv.Atoi(v.text(name))
	return value
}
func (v configValues) duration(name string) time.Duration {
	value, _ := time.ParseDuration(v.text(name))
	return value
}

func build(role string, fs *flag.FlagSet, stats *tunnel.Stats) (definition, error) {
	values := configValues{flags: fs}
	d := definition{listen: values.text("listen")}
	if role == "client" && fs.Lookup("proxies") != nil && strings.TrimSpace(values.text("proxies")) != "" {
		secret, err := cli.LoadKey(values.text("key-file"))
		if err != nil {
			return d, err
		}
		d.proxies, err = values.multiClientConfigs(secret, values.options(stats))
		if err != nil {
			return d, err
		}
		addresses := make([]string, len(d.proxies))
		for index, route := range d.proxies {
			addresses[index] = route.Listen
		}
		d.listen = strings.Join(addresses, ",")
		return d, nil
	}
	_, port, err := net.SplitHostPort(d.listen)
	if err != nil {
		return d, err
	}
	if p, err := net.LookupPort("tcp", port); err != nil || p < 0 || p > 65535 {
		return d, errors.New("invalid listen port")
	}
	secret, err := cli.LoadKey(values.text("key-file"))
	if err != nil {
		return d, err
	}
	options := values.options(stats)
	if role == "client" {
		d.client, err = values.clientConfig(secret, options)
	} else {
		d.server, d.tls, err = values.serverConfig(secret, options)
	}
	return d, err
}

func (v configValues) options(stats *tunnel.Stats) tunnel.Options {
	return tunnel.Options{
		Stats:                stats,
		BufferBytes:          v.integer("buffer-bytes"),
		MaxStreams:           v.integer("max-streams"),
		Heartbeat:            v.duration("heartbeat"),
		HeartbeatTimeout:     v.duration("heartbeat-timeout"),
		NoiseSuppressTraffic: !v.boolean("noise-traffic"),
		NoiseSuppressIdle:    !v.boolean("noise-idle"),
		NoiseSuppressEmpty:   !v.boolean("noise-empty"),
		NoiseIdleAfter:       v.duration("noise-idle-after"),
		NoiseInterval:        v.duration("noise-interval"),
		NoiseIntervalMin:     v.duration("noise-interval-min"),
		NoiseIntervalMax:     v.duration("noise-interval-max"),
		NoiseMinBytes:        v.integer("noise-min-bytes"),
		NoiseMaxBytes:        v.integer("noise-max-bytes"),
	}
}

func (v configValues) clientConfig(secret []byte, options tunnel.Options) (*tunnel.ClientConfig, error) {
	tlsConfig, err := clientTLSConfig(v.text("tls-ca"))
	if err != nil {
		return nil, err
	}
	config := v.clientSettings(secret, options)
	config.TLSConfig = tlsConfig
	if _, err := tunnel.NewClient(config); err != nil {
		return nil, err
	}
	return &config, nil
}

func (v configValues) clientSettings(secret []byte, options tunnel.Options) tunnel.ClientConfig {
	return tunnel.ClientConfig{
		Key:            secret,
		Password:       v.text("password"),
		URL:            v.text("server"),
		Addr:           v.text("addr"),
		OmitAddr:       v.text("send-addr") == "false",
		ReconnectMin:   v.duration("reconnect-min"),
		ReconnectMax:   v.duration("reconnect-max"),
		OfflineTimeout: v.duration("offline-timeout"),
		Options:        options,
	}
}

func clientTLSConfig(caPath string) (*tls.Config, error) {
	config := &tls.Config{MinVersion: tls.VersionTLS12}
	if caPath == "" {
		return config, nil
	}
	pem, err := os.ReadFile(caPath)
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
	config.RootCAs = roots
	return config, nil
}

func (v configValues) serverConfig(secret []byte, options tunnel.Options) (*tunnel.ServerConfig, *tls.Config, error) {
	tlsConfig, err := serverTLSConfig(v.text("tls-cert"), v.text("tls-key"))
	if err != nil {
		return nil, nil, err
	}
	config := tunnel.ServerConfig{
		Key:         secret,
		Password:    v.text("password"),
		Target:      v.text("target"),
		Path:        v.text("path"),
		SessionTTL:  v.duration("session-ttl"),
		MaxSessions: v.integer("max-sessions"),
		Options:     options,
	}
	if raw := strings.TrimSpace(v.text("allowed-targets")); raw != "" {
		for _, target := range strings.Split(raw, ",") {
			config.AllowedTargets = append(config.AllowedTargets, strings.TrimSpace(target))
		}
	}
	// NewServer 完成协议参数校验；这个临时实例不能留下清理协程。
	server, err := tunnel.NewServer(config)
	if err != nil {
		return nil, nil, err
	}
	server.Close()
	return &config, tlsConfig, nil
}

func serverTLSConfig(certPath, keyPath string) (*tls.Config, error) {
	if (certPath == "") != (keyPath == "") {
		return nil, errors.New("both TLS certificate and key are required")
	}
	if certPath == "" {
		return nil, nil
	}
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}, nil
}
