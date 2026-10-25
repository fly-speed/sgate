package tunnel

import "time"

type noiseActivity byte

const (
	noiseEmpty noiseActivity = iota
	noiseTraffic
	noiseIdle
)

// activityLocked 按隧道内全部业务流的双向数据判断；ACK、心跳、噪音不延长忙碌窗口。
func (s *session) activityLocked(now time.Time, idleAfter time.Duration) noiseActivity {
	if len(s.streams) == 0 {
		return noiseEmpty
	}
	if !s.lastBusiness.IsZero() && now.Sub(s.lastBusiness) < idleAfter {
		return noiseTraffic
	}
	return noiseIdle
}

func (s *session) noiseAllowed(options Options, now time.Time) bool {
	s.mu.Lock()
	state := s.activityLocked(now, options.NoiseIdleAfter)
	s.mu.Unlock()
	switch state {
	case noiseEmpty:
		return !options.NoiseSuppressEmpty
	case noiseTraffic:
		return !options.NoiseSuppressTraffic
	default:
		return !options.NoiseSuppressIdle
	}
}

func (s *session) noteBusiness() {
	s.mu.Lock()
	s.lastBusiness = time.Now()
	s.mu.Unlock()
}

func (o Options) sameNoisePolicy(next Options) bool {
	return o.NoiseSuppressTraffic == next.NoiseSuppressTraffic &&
		o.NoiseSuppressIdle == next.NoiseSuppressIdle &&
		o.NoiseSuppressEmpty == next.NoiseSuppressEmpty && o.NoiseIdleAfter == next.NoiseIdleAfter
}
