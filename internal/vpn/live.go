package vpn

import (
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"sgate/internal/cli"
)

type LiveConfig struct {
	Noise        NoiseConfig
	QueueMaxAge  time.Duration
	MaxClients   int
	AllowPrivate bool
}
type liveValue struct {
	config  LiveConfig
	changed chan struct{}
}
type LiveSettings struct {
	value atomic.Pointer[liveValue]
	mu    sync.Mutex
}

func LiveFields(role string) []string {
	keys := []string{"vpn-noise-rate", "vpn-noise-traffic", "vpn-noise-idle", "vpn-noise-idle-after", "vpn-queue-max-age"}
	if role == "server" {
		keys = append(keys, "vpn-max-clients", "vpn-allow-private")
	}
	return keys
}
func LiveFromConfig(c Config) LiveConfig {
	return LiveConfig{Noise: c.Noise, QueueMaxAge: c.QueueMaxAge, MaxClients: c.MaxClients, AllowPrivate: c.AllowPrivate}
}
func normalizeLive(c LiveConfig) (LiveConfig, error) {
	if c.Noise.Rate < 0 || c.Noise.Rate > maxNoiseRate || c.Noise.IdleAfter < 0 {
		return c, errors.New("invalid VPN live noise settings")
	}
	if c.Noise.IdleAfter == 0 {
		c.Noise.IdleAfter = time.Second
	}
	if c.QueueMaxAge < 0 || c.QueueMaxAge > time.Minute {
		return c, errors.New("VPN queue maximum age must be 0..1m")
	}
	if c.MaxClients == 0 {
		c.MaxClients = 64
	}
	if c.MaxClients < 1 || c.MaxClients > 254 {
		return c, errors.New("VPN max-clients must be 1..254")
	}
	return c, nil
}
func NewLiveSettings(c LiveConfig) (*LiveSettings, error) {
	v, err := normalizeLive(c)
	if err != nil {
		return nil, err
	}
	live := &LiveSettings{}
	live.value.Store(&liveValue{config: v, changed: make(chan struct{})})
	return live, nil
}
func (l *LiveSettings) Snapshot() LiveConfig { return l.value.Load().config }
func (l *LiveSettings) Watch() (LiveConfig, <-chan struct{}) {
	v := l.value.Load()
	return v.config, v.changed
}
func (l *LiveSettings) Update(c LiveConfig) error {
	v, err := normalizeLive(c)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.Snapshot() == v {
		return nil
	}
	previous := l.value.Load()
	l.value.Store(&liveValue{config: v, changed: make(chan struct{})})
	close(previous.changed)
	return nil
}
func WithLiveValues(c LiveConfig, values map[string]string, keys []string) (LiveConfig, error) {
	defaults := map[string]string{}
	for _, s := range Settings("server") {
		defaults[s.Key] = s.Default
	}
	for _, key := range keys {
		value, ok := values[key]
		if !ok {
			value = defaults[key]
		}
		var err error
		switch key {
		case "vpn-noise-rate":
			c.Noise.Rate, err = cli.ParseByteSize(value)
		case "vpn-noise-traffic":
			c.Noise.Traffic, err = strconv.ParseBool(value)
		case "vpn-noise-idle":
			c.Noise.Idle, err = strconv.ParseBool(value)
		case "vpn-noise-idle-after":
			c.Noise.IdleAfter, err = time.ParseDuration(value)
		case "vpn-queue-max-age":
			c.QueueMaxAge, err = time.ParseDuration(value)
		case "vpn-max-clients":
			c.MaxClients, err = strconv.Atoi(value)
		case "vpn-allow-private":
			c.AllowPrivate, err = strconv.ParseBool(value)
		}
		if err != nil {
			return c, errors.New("invalid VPN live setting: " + key)
		}
	}
	return normalizeLive(c)
}
func (c *Config) setLive(v LiveConfig) {
	c.Noise = v.Noise
	c.QueueMaxAge = v.QueueMaxAge
	c.MaxClients = v.MaxClients
	c.AllowPrivate = v.AllowPrivate
}

// WithLiveConfig copies the prepared definition without changing static identity.
func (p *Prepared) WithLiveConfig(v LiveConfig) *Prepared {
	next := *p
	next.Config = p.Config
	next.Config.setLive(v)
	return &next
}
