// Package tunnel implements authenticated, encrypted, resumable TCP streams over WebSocket.
package tunnel

import (
	"errors"
	"log"
	"time"
)

type Options struct {
	BufferBytes  int
	MaxStreams   int
	DialTimeout  time.Duration
	WriteTimeout time.Duration
	Heartbeat    time.Duration
	Logf         func(string, ...any)
}

func (o Options) defaults() (Options, error) {
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
	// A TCP dial or write may temporarily occupy the receive loop.
	if o.Heartbeat*3 <= o.DialTimeout || o.Heartbeat*3 <= o.WriteTimeout {
		return o, errors.New("three heartbeat intervals must exceed dial and write timeouts")
	}
	return o, nil
}
