package app

import (
	"errors"
	"slices"

	"sgate/internal/tunnel"
)

func (d definition) liveConfig() tunnel.LiveConfig {
	if d.server != nil {
		return tunnel.LiveFromOptions(d.server.Options)
	}
	cfg := d.client
	if len(d.proxies) > 0 {
		cfg = &d.proxies[0].config
	}
	v := tunnel.LiveFromOptions(cfg.Options)
	v.ReconnectMin, v.ReconnectMax = cfg.ReconnectMin, cfg.ReconnectMax
	return v
}

// 复制配置结构；正在运行的读写协程只读取 LiveSettings，不修改其启动快照。
func (d definition) withLiveSettings(live *tunnel.LiveSettings) definition {
	if d.server != nil {
		v := *d.server
		v.Options.Live = live
		d.server = &v
	}
	if d.client != nil {
		v := *d.client
		v.Options.Live = live
		d.client = &v
	}
	d.proxies = slices.Clone(d.proxies)
	for n := range d.proxies {
		d.proxies[n].config.Options.Live = live
	}
	return d
}

func copyLiveFields(dst *tunnel.LiveConfig, src tunnel.LiveConfig, keys []string) {
	for _, key := range keys {
		switch key {
		case "noise-traffic":
			dst.NoiseSuppressTraffic = src.NoiseSuppressTraffic
		case "noise-idle":
			dst.NoiseSuppressIdle = src.NoiseSuppressIdle
		case "noise-empty":
			dst.NoiseSuppressEmpty = src.NoiseSuppressEmpty
		case "noise-idle-after":
			dst.NoiseIdleAfter = src.NoiseIdleAfter
		case "noise-interval":
			dst.NoiseInterval = src.NoiseInterval
		case "noise-interval-min":
			dst.NoiseIntervalMin = src.NoiseIntervalMin
		case "noise-interval-max":
			dst.NoiseIntervalMax = src.NoiseIntervalMax
		case "noise-min-bytes":
			dst.NoiseMinBytes = src.NoiseMinBytes
		case "noise-max-bytes":
			dst.NoiseMaxBytes = src.NoiseMaxBytes
		case "reconnect-min":
			dst.ReconnectMin = src.ReconnectMin
		case "reconnect-max":
			dst.ReconnectMax = src.ReconnectMax
		}
	}
}

func (d definition) withLiveConfig(v tunnel.LiveConfig) definition {
	update := func(o *tunnel.Options) {
		o.NoiseSuppressTraffic, o.NoiseSuppressIdle, o.NoiseSuppressEmpty = v.NoiseSuppressTraffic, v.NoiseSuppressIdle, v.NoiseSuppressEmpty
		o.NoiseIdleAfter = v.NoiseIdleAfter
		o.NoiseInterval, o.NoiseIntervalMin, o.NoiseIntervalMax = v.NoiseInterval, v.NoiseIntervalMin, v.NoiseIntervalMax
		o.NoiseMinBytes, o.NoiseMaxBytes = v.NoiseMinBytes, v.NoiseMaxBytes
	}
	d = d.withLiveSettings(nil)
	if d.server != nil {
		update(&d.server.Options)
	}
	if d.client != nil {
		update(&d.client.Options)
		d.client.ReconnectMin, d.client.ReconnectMax = v.ReconnectMin, v.ReconnectMax
	}
	for n := range d.proxies {
		cfg := &d.proxies[n].config
		update(&cfg.Options)
		cfg.ReconnectMin, cfg.ReconnectMax = v.ReconnectMin, v.ReconnectMax
	}
	return d
}

// 在线保存只更新本次提交的热参数，不能顺带应用其他已保存但待重启的设置。
func (c *controller) applyLive(keys []string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ctx.Err() != nil || c.restartPending {
		return errors.New("服务正在停止或重启，请稍后重试")
	}
	if c.worker == nil || c.worker.live == nil {
		return errors.New("代理未运行，不能立即应用")
	}
	select {
	case <-c.worker.done:
		return errors.New("代理已停止，不能立即应用")
	default:
	}
	next, err := c.load("")
	if err != nil {
		return err
	}
	value := c.current.liveConfig()
	copyLiveFields(&value, next.liveConfig(), keys)
	if err = c.worker.live.Update(value); err != nil {
		return err
	}
	c.current = c.current.withLiveConfig(value)
	c.lastError = ""
	return nil
}

func (c *controller) liveFields() []string {
	fields := tunnel.LiveFields()
	if c.role == "server" {
		fields = slices.DeleteFunc(fields, func(key string) bool { return key == "reconnect-min" || key == "reconnect-max" })
	}
	return fields
}
