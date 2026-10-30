package app

import (
	"context"
	"log"

	"sgate/internal/admin"
	"sgate/internal/tunnel"
)

func (c *controller) testProxy(ctx context.Context, mapping admin.ProxyMapping) (tunnel.ProbeResult, error) {
	// Copy already-effective credentials/TLS/send-addr settings under the lock.
	// A slow network probe must not block save/apply, login, or live statistics.
	c.mu.Lock()
	var config tunnel.ClientConfig
	switch {
	case c.role != "client":
		c.mu.Unlock()
		return proxyConfigFailure(mapping.Listen, "mapping tests require a client")
	case len(c.current.proxies) > 0:
		config = c.current.proxies[0].config
	case c.current.client != nil:
		config = *c.current.client
	default:
		c.mu.Unlock()
		return proxyConfigFailure(mapping.Listen, "no effective client configuration")
	}
	config.Username, config.Password = c.current.commonUsername, c.current.commonPassword
	c.mu.Unlock()
	if mapping.Username != nil {
		config.Username = *mapping.Username
	}
	if mapping.Password != nil {
		config.Password = *mapping.Password
	}
	config.URL, config.Addr = mapping.Server, mapping.Addr
	config.Options.Stats, config.Options.Live = nil, nil
	client, err := tunnel.NewClient(config)
	if err != nil {
		return proxyConfigFailure(mapping.Listen, err.Error())
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if c.ctx != nil {
		stop := context.AfterFunc(c.ctx, cancel)
		defer stop()
	}
	return client.Probe(ctx), nil
}

// NewClient validation errors contain fixed messages, never credential values.
func proxyConfigFailure(listen, reason string) (tunnel.ProbeResult, error) {
	log.Printf("mapping connectivity test failed: listen=%q stage=config reason=%q", listen, reason)
	return tunnel.ProbeResult{Stage: "config", Detail: "invalid effective client configuration or mapping", Error: reason}, nil
}
