package tunnel

import (
	"context"
	"crypto/hmac"
	"crypto/tls"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type ClientConfig struct {
	Key          []byte
	URL          string
	ReconnectMin time.Duration
	ReconnectMax time.Duration
	TLSConfig    *tls.Config
	Options      Options
}
type Client struct{ cfg ClientConfig }

func NewClient(cfg ClientConfig) (*Client, error) {
	if len(cfg.Key) != 32 {
		return nil, errors.New("client key must contain 32 bytes")
	}
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Host == "" || (u.Scheme != "ws" && u.Scheme != "wss") || u.User != nil || u.Fragment != "" {
		return nil, errors.New("server URL must be ws://host/path or wss://host/path")
	}
	if cfg.ReconnectMin == 0 {
		cfg.ReconnectMin = 250 * time.Millisecond
	}
	if cfg.ReconnectMax == 0 {
		cfg.ReconnectMax = 10 * time.Second
	}
	if cfg.ReconnectMin <= 0 || cfg.ReconnectMax < cfg.ReconnectMin {
		return nil, errors.New("invalid reconnect interval")
	}
	cfg.Options, err = cfg.Options.defaults()
	if err != nil {
		return nil, err
	}
	cfg.Key = append([]byte(nil), cfg.Key...)
	if cfg.TLSConfig != nil {
		cfg.TLSConfig = cfg.TLSConfig.Clone()
	}
	return &Client{cfg: cfg}, nil
}
func (c *Client) dial(ctx context.Context, id string, resume bool) (*websocket.Conn, *codec, int, error) {
	nonce := randomID()
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	r := "0"
	if resume {
		r = "1"
	}
	u, _ := url.Parse(c.cfg.URL)
	headers := http.Header{}
	headers.Set("X-Sgate-Session", id)
	headers.Set("X-Sgate-Nonce", nonce)
	headers.Set("X-Sgate-Time", ts)
	headers.Set("X-Sgate-Resume", r)
	headers.Set("X-Sgate-Auth", mac(c.cfg.Key, protocol, "client", id, nonce, ts, r, u.RequestURI()))
	d := websocket.Dialer{Proxy: http.ProxyFromEnvironment, HandshakeTimeout: c.cfg.Options.DialTimeout, TLSClientConfig: c.cfg.TLSConfig, ReadBufferSize: chunkSize, WriteBufferSize: chunkSize}
	ws, response, err := d.DialContext(ctx, c.cfg.URL, headers)
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
			response.Body.Close()
		}
		return nil, nil, status, err
	}
	serverNonce := response.Header.Get("X-Sgate-Nonce")
	expected := mac(c.cfg.Key, protocol, "server", id, nonce, serverNonce, ts)
	if !validID(serverNonce) || !hmac.Equal([]byte(expected), []byte(response.Header.Get("X-Sgate-Auth"))) {
		ws.Close()
		return nil, nil, 0, errors.New("server authentication failed")
	}
	return ws, newCodec(c.cfg.Key, id, nonce, serverNonce, true), http.StatusSwitchingProtocols, nil
}

// Run owns listener until ctx is canceled. Existing TCP streams survive transport
// failure while the server retains its session and the target keeps its socket open.
func (c *Client) Run(ctx context.Context, listener net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	current := newSession(ctx, "", c.cfg.Options)
	acceptErr := make(chan error, 1)
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			conn, err := listener.Accept()
			if err != nil {
				acceptErr <- err
				cancel()
				return
			}
			mu.Lock()
			s := current
			mu.Unlock()
			if err = s.addLocal(conn); err != nil {
				c.cfg.Options.Logf("TCP connection rejected: %v", err)
			}
		}
	}()
	shutdownDone := make(chan struct{})
	go func() {
		<-ctx.Done()
		listener.Close()
		mu.Lock()
		current.close()
		mu.Unlock()
		close(shutdownDone)
	}()
	defer func() { cancel(); <-shutdownDone; <-acceptDone }()
	id := randomID()
	resume := false
	backoff := c.cfg.ReconnectMin
	for ctx.Err() == nil {
		current.mu.Lock()
		closed := current.closed
		current.mu.Unlock()
		if closed {
			if ctx.Err() != nil {
				break
			}
			mu.Lock()
			current = newSession(ctx, "", c.cfg.Options)
			mu.Unlock()
			id, resume = randomID(), false
		}
		ws, crypt, status, err := c.dial(ctx, id, resume)
		if status == http.StatusGone && resume {
			// A server restart/expired session cannot preserve the backend TCP sockets.
			mu.Lock()
			current.close()
			current = newSession(ctx, "", c.cfg.Options)
			mu.Unlock()
			id = randomID()
			resume = false
			backoff = c.cfg.ReconnectMin
			c.cfg.Options.Logf("server session lost; existing TCP connections closed")
			continue
		}
		if err == nil {
			resume = true
			backoff = c.cfg.ReconnectMin
			c.cfg.Options.Logf("tunnel connected")
			err = current.transport(ws, crypt)
		}
		if ctx.Err() != nil {
			break
		}
		delay := backoff/2 + time.Duration(rand.Int64N(int64(backoff/2)+1))
		c.cfg.Options.Logf("tunnel unavailable: %v; retry in %s", err, delay)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
		if backoff < c.cfg.ReconnectMax/2 {
			backoff *= 2
		} else {
			backoff = c.cfg.ReconnectMax
		}
	}
	select {
	case err := <-acceptErr:
		if !errors.Is(err, net.ErrClosed) {
			return fmt.Errorf("TCP listener: %w", err)
		}
	default:
	}
	return nil
}
