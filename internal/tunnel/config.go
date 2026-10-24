// Package tunnel implements authenticated, encrypted, resumable TCP streams over WebSocket.
package tunnel

import (
	"errors"
	"log"
	"time"
)

type Options struct {
	BufferBytes      int
	MaxStreams       int
	DialTimeout      time.Duration
	WriteTimeout     time.Duration
	Heartbeat        time.Duration
	HeartbeatTimeout time.Duration
	NoiseInterval    time.Duration
	NoiseIntervalMin time.Duration
	NoiseIntervalMax time.Duration
	NoiseMinBytes    int
	NoiseMaxBytes    int
	Logf             func(string, ...any)
}

func (o Options) defaults() (Options, error) {
	if o.NoiseIntervalMin != 0 || o.NoiseIntervalMax != 0 {
		if o.NoiseInterval != 0 || o.NoiseIntervalMin <= 0 || o.NoiseIntervalMax < o.NoiseIntervalMin {
			return o, errors.New("noise interval range requires 0 < min <= max and cannot be combined with noise-interval")
		}
	}
	if o.NoiseMinBytes == 0 {
		o.NoiseMinBytes = 1024
	}
	if o.NoiseMaxBytes == 0 {
		o.NoiseMaxBytes = 10 * 1024
	}
	if o.NoiseInterval < 0 || o.NoiseMinBytes < 1 || o.NoiseMaxBytes < o.NoiseMinBytes || o.NoiseMaxBytes > chunkSize {
		return o, errors.New("invalid noise options: interval must be nonnegative and 1 <= min <= max <= 16384 bytes")
	}
	if o.BufferBytes == 0 {
		o.BufferBytes = 4 * 1024 * 1024
	}
	if o.MaxStreams == 0 {
		o.MaxStreams = 128
	}
	if o.DialTimeout == 0 {
		o.DialTimeout = 10 * time.Second
	}
	if o.WriteTimeout == 0 {
		o.WriteTimeout = 15 * time.Second
	}
	if o.Heartbeat == 0 {
		o.Heartbeat = 20 * time.Second
	}
	if o.Logf == nil {
		o.Logf = log.Printf
	}
	if o.BufferBytes < chunkSize+frameHeader || o.MaxStreams < 1 || o.DialTimeout <= 0 || o.WriteTimeout <= 0 || o.Heartbeat <= 0 {
		return o, errors.New("invalid buffer, stream limit, or timeout")
	}
	if o.HeartbeatTimeout == 0 {
		if o.Heartbeat > time.Duration(1<<63-1)/3 {
			return o, errors.New("heartbeat interval is too large")
		}
		o.HeartbeatTimeout = o.Heartbeat * 3
	}
	// Leave enough time for both periodic probes and local TCP work.
	if o.HeartbeatTimeout <= o.Heartbeat || o.HeartbeatTimeout <= o.DialTimeout || o.HeartbeatTimeout <= o.WriteTimeout {
		return o, errors.New("heartbeat timeout must exceed heartbeat interval, dial timeout and write timeout")
	}
	return o, nil
}
