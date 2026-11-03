package app

import (
	"sgate/internal/tunnel"
	"time"
)

// noiseStatus 来自正在运行的代理定义，而非尚未应用的磁盘配置。
func (d definition) noiseStatus() map[string]any {
	var options tunnel.Options
	switch {
	case d.server != nil:
		options = d.server.Options
	case d.client != nil:
		options = d.client.Options
	case len(d.proxies) > 0:
		options = d.proxies[0].config.Options
	}
	mode := "off"
	if options.NoiseIntervalMin > 0 {
		mode = "random"
	} else if options.NoiseInterval > 0 {
		mode = "fixed"
	}
	minSize, maxSize := options.NoiseMinBytes, options.NoiseMaxBytes
	if minSize == 0 {
		minSize = 1024
	}
	if maxSize == 0 {
		maxSize = 10 * 1024
	}
	idleAfter := options.NoiseIdleAfter
	if idleAfter == 0 {
		idleAfter = time.Second
	}
	return map[string]any{
		"traffic": !options.NoiseSuppressTraffic, "idle": !options.NoiseSuppressIdle,
		"empty": !options.NoiseSuppressEmpty, "idle_after": idleAfter.String(),
		"mode": mode, "interval": options.NoiseInterval.String(),
		"interval_min": options.NoiseIntervalMin.String(), "interval_max": options.NoiseIntervalMax.String(),
		"min_bytes": minSize, "max_bytes": maxSize,
		"quic_unit": "bytes_per_second", "quic_rate_hold_min": "1s",
	}
}
