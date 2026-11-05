package vpn

import (
	"context"
	"crypto/rand"
	"sync/atomic"
	"time"
)

const noiseTick = 5 * time.Millisecond
const noisePressureHold = time.Second
const noiseSlowWrite = 10 * time.Millisecond
const maxNoiseRate = 10 * 1024 * 1024
const paddingChallengePrefix = probeChallengePrefix + "padding-v1:"

type NoiseConfig struct {
	Rate      int
	Traffic   bool
	Idle      bool
	IdleAfter time.Duration
}
type flowContextKey struct{}
type datagramFlow struct {
	queued, sent   atomic.Int64
	cwnd, inflight atomic.Int64
	ready          chan struct{}
}

func newDatagramFlow() *datagramFlow   { return &datagramFlow{ready: make(chan struct{}, 1)} }
func (f *datagramFlow) pending() int64 { return f.queued.Load() - f.sent.Load() }
func (f *datagramFlow) signal() {
	select {
	case f.ready <- struct{}{}:
	default:
	}
}

func (f *datagramFlow) allowNoise() bool {
	if f == nil || f.queued.Load()-f.sent.Load() >= 4 {
		return false
	}
	window := f.cwnd.Load()
	return window == 0 || f.inflight.Load()+2400 < window
}
func (p *peer) sendDatagram(data []byte) error {
	if p.flow != nil {
		p.flow.queued.Add(1)
	}
	err := p.conn.SendDatagram(data)
	if err != nil && p.flow != nil {
		p.flow.queued.Add(-1)
	}
	return err
}
func (p *peer) businessActivity() { p.lastBusiness.Store(time.Since(p.started).Nanoseconds() + 1) }
func (p *peer) noiseAllowed(now time.Time, c NoiseConfig) bool {
	last := p.lastBusiness.Load()
	active := last > 0 && now.Sub(p.started)-time.Duration(last-1) < c.IdleAfter
	return active && c.Traffic || !active && c.Idle
}

// Transports without QUIC admission data use bulk traffic and slow writes as
// local pressure signals. A hold covers gaps between bursts and TCP recovery.
func (p *peer) noteNoisePressure(now time.Time) {
	if p.flow == nil {
		storeMax(&p.noisePausedUntil, uint64(now.Sub(p.started)+noisePressureHold))
	}
}

func (p *peer) noiseCanSend(now time.Time) bool {
	if p.queue.len() != 0 {
		return false
	}
	if p.flow != nil {
		return p.flow.allowNoise()
	}
	return now.Sub(p.started) >= time.Duration(p.noisePausedUntil.Load())
}

type noisePacer struct {
	last   time.Time
	credit int64
}

func (n *noisePacer) budget(now time.Time, rate int, allowed bool) int {
	elapsed := now.Sub(n.last)
	n.last = now
	if !allowed {
		n.credit = 0
		return 0
	}
	// Scheduling delays, offline periods and congestion never create catch-up bursts.
	if elapsed < 0 {
		elapsed = 0
	}
	if elapsed > noiseTick {
		elapsed = noiseTick
	}
	n.credit += int64(rate) * int64(elapsed)
	budget := int(n.credit / int64(time.Second))
	// Keep low-rate packet overhead reasonable without accumulating congestion debt.
	goal := max(1, min(fragmentPayload, rate/20))
	if budget < goal {
		return 0
	}
	n.credit %= int64(time.Second)
	return budget
}
func (p *peer) sendNoise(now time.Time, c Config, pacer *noisePacer) error {
	allowed := p.noiseAllowed(now, c.Noise) && p.noiseCanSend(now)
	budget := pacer.budget(now, c.Noise.Rate, allowed)
	if !allowed {
		c.Stats.noiseSkipped.Add(1)
		return nil
	}
	for budget > 0 {
		if !p.noiseCanSend(now) || (c.Live != nil && c.Live.Snapshot().Noise != c.Noise) {
			c.Stats.noiseSkipped.Add(1)
			break
		}
		size := min(budget, fragmentPayload)
		data := make([]byte, 2+size)
		data[0] = 1
		data[1] = 1
		if _, err := rand.Read(data[2:]); err != nil {
			return err
		}
		began := time.Now()
		if err := p.sendDatagram(data); err != nil {
			return err
		}
		c.Stats.noiseTX.Add(uint64(size))
		if ended := time.Now(); ended.Sub(began) >= noiseSlowWrite {
			p.noteNoisePressure(ended)
			now = ended
		}
		budget -= size
	}
	return nil
}

// Sending at most one IP packet beyond this threshold bounds the QUIC FIFO
// to five DATAGRAMs with the current two-fragment wire format.
func (p *peer) waitForAdmission(ctx context.Context, stats *Stats) error {
	if p.flow == nil {
		return nil
	}
	for p.flow.pending() >= 4 {
		began := time.Now()
		stats.admissionBlocked.Add(1)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.flow.ready:
		}
		stats.admissionWait.Add(uint64(time.Since(began)))
	}
	return ctx.Err()
}
