package tunnel

import (
	"net"
	"time"
)

// QUIC interprets noise-min/max-bytes as payload bytes per second. Keep at
// most one small frame due: pauses, congestion and reconnects never bank credit.
// Rate choices last at least one second; interval settings can hold them longer.
type quicNoisePacer struct {
	options            Options
	initialized        bool
	nextSend, nextRate time.Time
	size               int
	period             time.Duration
}

func (p *quicNoisePacer) poll(now time.Time, o Options, allowed bool) (int, time.Duration, error) {
	previous := p.options
	if previous.NoiseMinBytes != o.NoiseMinBytes || previous.NoiseMaxBytes != o.NoiseMaxBytes ||
		previous.NoiseInterval != o.NoiseInterval || previous.NoiseIntervalMin != o.NoiseIntervalMin || previous.NoiseIntervalMax != o.NoiseIntervalMax || !previous.sameNoisePolicy(o) {
		p.initialized = false
	}
	p.options = o
	if (o.NoiseInterval == 0 && o.NoiseIntervalMin == 0) || (o.NoiseSuppressTraffic && o.NoiseSuppressIdle && o.NoiseSuppressEmpty) {
		p.initialized = false
		return 0, 0, nil
	}
	if !allowed {
		p.initialized = false
		// A busy flow can become idle without another stream/control event.
		return 0, 100 * time.Millisecond, nil
	}
	if !p.initialized || !now.Before(p.nextRate) {
		rate, err := o.noiseSize()
		if err != nil {
			return 0, 0, err
		}
		hold, err := o.noiseDelay()
		if err != nil {
			return 0, 0, err
		}
		if hold <= 0 {
			p.initialized = false
			return 0, 0, nil
		}
		p.size = min(chunkSize, max(1, rate/50))
		p.period = time.Duration((int64(p.size)*int64(time.Second) + int64(rate) - 1) / int64(rate))
		p.nextRate = now.Add(max(time.Second, hold))
		if !p.initialized {
			p.nextSend = now.Add(p.period)
		}
		p.initialized = true
	}
	if !now.Before(p.nextSend) {
		return p.size, 0, nil
	}
	return 0, min(p.nextSend.Sub(now), p.nextRate.Sub(now)), nil
}
func (p *quicNoisePacer) sent(now time.Time) { p.nextSend = now.Add(p.period) }

func isQUICStream(conn net.Conn) bool {
	for conn != nil {
		switch c := conn.(type) {
		case *quicStreamConn:
			return true
		case *trafficConn:
			conn = c.Conn
		default:
			if c, ok := conn.(interface{ NetConn() net.Conn }); ok {
				conn = c.NetConn()
			} else {
				return false
			}
		}
	}
	return false
}
