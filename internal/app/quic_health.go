package app

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"sgate/internal/tunnel"
)

type quicHealth struct {
	Enabled       bool   `json:"enabled"`
	Healthy       bool   `json:"healthy"`
	State         string `json:"state"`
	Address       string `json:"address,omitempty"`
	Failures      uint64 `json:"failures"`
	Recoveries    uint64 `json:"recoveries"`
	LastError     string `json:"last_error,omitempty"`
	LastFailureMS int64  `json:"last_failure_ms,omitempty"`
}
type quicWorker struct {
	mu       sync.Mutex
	health   quicHealth
	listener net.Listener
	server   *http.Server
	cancel   context.CancelFunc
	done     chan struct{}
	once     sync.Once
}

func (q *quicWorker) snapshot() quicHealth {
	if q == nil {
		return quicHealth{State: "disabled", Healthy: true}
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.health
}
func (q *quicWorker) stop() {
	if q == nil {
		return
	}
	q.once.Do(func() {
		q.cancel()
		q.mu.Lock()
		server := q.server
		q.health.State = "stopped"
		q.health.Healthy = false
		q.mu.Unlock()
		if server != nil {
			server.Close()
		}
	})
	<-q.done
}
func newQUICHTTP(handler http.Handler) *http.Server {
	return &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 16 * 1024}
}
func (i *instance) startQUIC(d definition, handler http.Handler, config *tls.Config, stats *tunnel.Stats) error {
	if d.quicListen == "" {
		return nil
	}
	listener, err := tunnel.ListenQUIC(d.quicListen, config, d.server.Options, handler)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	server := newQUICHTTP(handler)
	q := &quicWorker{health: quicHealth{Enabled: true, Healthy: true, State: "listening", Address: listener.Addr().String()}, listener: listener, server: server, cancel: cancel, done: make(chan struct{})}
	i.quic = q
	i.quicListener = listener
	i.quicHTTP = server
	i.quicDone = q.done
	go func() {
		defer close(q.done)
		address := listener.Addr().String()
		retry := 100 * time.Millisecond
		for {
			err := server.Serve(stats.Listener(listener))
			server.Close()
			listener.Close()
			if ctx.Err() != nil {
				return
			}
			q.mu.Lock()
			q.health.Healthy = false
			q.health.State = "recovering"
			q.health.Failures++
			q.health.LastError = fmt.Sprint(err)
			q.health.LastFailureMS = time.Now().UnixMilli()
			q.mu.Unlock()
			if logger := d.server.Options.Logf; logger != nil {
				logger("QUIC listener unavailable: %v", err)
			} else {
				log.Printf("QUIC listener unavailable: %v", err)
			}
			for {
				timer := time.NewTimer(retry)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
				listener, err = tunnel.ListenQUIC(address, config, d.server.Options, handler)
				if err != nil {
					q.mu.Lock()
					q.health.Failures++
					q.health.LastError = err.Error()
					q.health.LastFailureMS = time.Now().UnixMilli()
					q.mu.Unlock()
					retry = min(retry*2, 5*time.Second)
					continue
				}
				server = newQUICHTTP(handler)
				q.mu.Lock()
				if ctx.Err() != nil {
					q.mu.Unlock()
					listener.Close()
					return
				}
				q.listener, q.server = listener, server
				q.health.State = "listening"
				q.health.Healthy = true
				q.health.Recoveries++
				q.mu.Unlock()
				retry = 100 * time.Millisecond
				break
			}
		}
	}()
	return nil
}
func (i *instance) stopQUIC() {
	if i.quic != nil {
		i.quic.stop()
	}
}
