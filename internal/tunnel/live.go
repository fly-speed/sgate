package tunnel

import (
	"errors"
	"sync"
	"time"
)

// LiveConfig 只包含不改变协议、业务连接和重放状态的参数。
// 同一次提交原子替换全部参数，避免读到混合的随机上下限。
type LiveConfig struct {
	NoiseSuppressTraffic, NoiseSuppressIdle, NoiseSuppressEmpty bool
	NoiseIdleAfter                                              time.Duration
	NoiseInterval, NoiseIntervalMin, NoiseIntervalMax           time.Duration
	NoiseMinBytes, NoiseMaxBytes                                int
	ReconnectMin, ReconnectMax                                  time.Duration
}

func LiveFields() []string {
	return []string{
		"noise-interval", "noise-interval-min", "noise-interval-max",
		"noise-min-bytes", "noise-max-bytes",
		"noise-traffic", "noise-idle", "noise-empty", "noise-idle-after",
		"reconnect-min", "reconnect-max",
	}
}

var errLiveReconnect = errors.New("invalid reconnect interval")

type LiveSettings struct {
	mu      sync.Mutex
	value   LiveConfig
	changed chan struct{}
}

func NewLiveSettings(value LiveConfig) (*LiveSettings, error) {
	s := &LiveSettings{changed: make(chan struct{})}
	if err := s.Update(value); err != nil {
		return nil, err
	}
	return s, nil
}

func (v LiveConfig) normalized() (LiveConfig, error) {
	o, err := v.apply(Options{}).defaults()
	if err != nil {
		return v, err
	}
	v.NoiseMinBytes, v.NoiseMaxBytes = o.NoiseMinBytes, o.NoiseMaxBytes
	v.NoiseIdleAfter = o.NoiseIdleAfter
	if v.ReconnectMin == 0 {
		v.ReconnectMin = 250 * time.Millisecond
	}
	if v.ReconnectMax == 0 {
		v.ReconnectMax = 10 * time.Second
	}
	if v.ReconnectMin <= 0 || v.ReconnectMax < v.ReconnectMin {
		return v, errLiveReconnect
	}
	return v, nil
}

func (s *LiveSettings) Update(value LiveConfig) error {
	value, err := value.normalized()
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if value != s.value {
		s.value = value
		close(s.changed)
		s.changed = make(chan struct{})
	}
	return nil
}

func (s *LiveSettings) Snapshot() (LiveConfig, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.value, s.changed
}

func LiveFromOptions(o Options) LiveConfig {
	return LiveConfig{
		NoiseSuppressTraffic: o.NoiseSuppressTraffic, NoiseSuppressIdle: o.NoiseSuppressIdle,
		NoiseSuppressEmpty: o.NoiseSuppressEmpty, NoiseIdleAfter: o.NoiseIdleAfter,
		NoiseInterval: o.NoiseInterval, NoiseIntervalMin: o.NoiseIntervalMin,
		NoiseIntervalMax: o.NoiseIntervalMax,
		NoiseMinBytes:    o.NoiseMinBytes, NoiseMaxBytes: o.NoiseMaxBytes,
	}
}

func (v LiveConfig) apply(o Options) Options {
	o.NoiseSuppressTraffic, o.NoiseSuppressIdle, o.NoiseSuppressEmpty = v.NoiseSuppressTraffic, v.NoiseSuppressIdle, v.NoiseSuppressEmpty
	o.NoiseIdleAfter = v.NoiseIdleAfter
	o.NoiseInterval, o.NoiseIntervalMin, o.NoiseIntervalMax = v.NoiseInterval, v.NoiseIntervalMin, v.NoiseIntervalMax
	o.NoiseMinBytes, o.NoiseMaxBytes = v.NoiseMinBytes, v.NoiseMaxBytes
	return o
}

func (o Options) liveSnapshot() (Options, <-chan struct{}) {
	if o.Live == nil {
		return o, nil
	}
	value, changed := o.Live.Snapshot()
	return value.apply(o), changed
}
