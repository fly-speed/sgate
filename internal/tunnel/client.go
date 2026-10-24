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
	Key            []byte
	Password       string
	URL            string
	ReconnectMin   time.Duration
	ReconnectMax   time.Duration
	OfflineTimeout time.Duration
	TLSConfig      *tls.Config
	Options        Options
}
type Client struct{ cfg ClientConfig }

func NewClient(cfg ClientConfig) (*Client, error) {
	if len(cfg.Key) != 32 {
		return nil, errors.New("client key must contain 32 bytes")
	}
	if err := validatePassword(cfg.Password); err != nil {
		return nil, err
	}
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Host == "" || (u.Scheme != "ws" && u.Scheme != "wss") || u.User != nil ||
		u.Fragment != "" {
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
	if cfg.OfflineTimeout == 0 {
		cfg.OfflineTimeout = 2 * time.Minute
	}
	if cfg.OfflineTimeout < 0 {
		return nil, errors.New("offline timeout must be positive")
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
	headers, err := requestHeaders(c.cfg.Key, c.cfg.Password, id, nonce, ts, r, u.RequestURI())
	if err != nil {
		return nil, nil, 0, err
	}
	d := websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: c.cfg.Options.DialTimeout,
		TLSClientConfig:  c.cfg.TLSConfig,
		ReadBufferSize:   chunkSize,
		WriteBufferSize:  chunkSize,
	}
	if stats := c.cfg.Options.Stats; stats != nil {
		d.NetDialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := (&net.Dialer{KeepAlive: 30 * time.Second}).DialContext(ctx, network, address)
			if err != nil {
				return nil, err
			}
			return stats.Conn(conn), nil
		}
	}
	ws, response, err := d.DialContext(ctx, c.cfg.URL, headers)
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
			response.Body.Close()
			if status == http.StatusGone {
				serverNonce := response.Header.Get("X-Sgate-Nonce")
				expected := mac(c.cfg.Key, protocol, "gone", id, nonce, serverNonce, ts, headers.Get(passwordHeader))
				if !validID(serverNonce) || !hmac.Equal([]byte(expected), []byte(response.Header.Get("X-Sgate-Auth"))) {
					return nil, nil, 0, errors.New("unauthenticated session expiry response")
				}
			}
		}
		return nil, nil, status, err
	}
	serverNonce := response.Header.Get("X-Sgate-Nonce")
	expected := mac(c.cfg.Key, protocol, "server", id, nonce, serverNonce, ts, headers.Get(passwordHeader))
	if !validID(serverNonce) || !hmac.Equal([]byte(expected), []byte(response.Header.Get("X-Sgate-Auth"))) {
		ws.Close()
		return nil, nil, 0, errors.New("server authentication failed")
	}
	return ws, newCodec(c.cfg.Key, id, nonce, serverNonce, true), http.StatusSwitchingProtocols, nil
}

// Run owns listener until ctx is canceled. Existing TCP streams survive transport
// failure while the server retains its session and the target keeps its socket open.
func (c *Client) Run(ctx context.Context, listener net.Listener) error {
	return c.run(ctx, listener, &clientSessions{})
}

// clientSessions serializes listener admission with session replacement.
type clientSessions struct {
	mu      sync.Mutex
	current *session
}

func (c *Client) run(ctx context.Context, listener net.Listener, state *clientSessions) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	mu := &state.mu
	mu.Lock()
	state.current = newSession(ctx, "", c.cfg.Options)
	mu.Unlock()
	acceptErr, stopLocal := c.startLocalListener(ctx, cancel, listener, state)
	defer stopLocal()
	c.reconnect(ctx, state)
	select {
	case err := <-acceptErr:
		if !errors.Is(err, net.ErrClosed) {
			return fmt.Errorf("TCP listener: %w", err)
		}
	default:
	}
	return nil
}

// 本地监听、关闭通知与会话替换共用 state.mu；stop 必须等待两个协程都退出。
func (c *Client) startLocalListener(
	ctx context.Context,
	cancel context.CancelFunc,
	listener net.Listener,
	state *clientSessions,
) (
	<-chan error,
	func(),
) {
	mu := &state.mu
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
			s := state.current
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
		state.current.close()
		mu.Unlock()
		close(shutdownDone)
	}()
	stop := func() {
		cancel()
		<-shutdownDone
		// No more replacements are possible once the run loop has returned.
		mu.Lock()
		state.current.close()
		mu.Unlock()
		<-acceptDone
	}
	return acceptErr, stop
}

// reconnect 只替换传输；离线窗口耗尽或收到可信 410 时才销毁业务会话。
func (c *Client) reconnect(ctx context.Context, state *clientSessions) {
	mu := &state.mu
	id := randomID()
	resume := false
	backoff := c.cfg.ReconnectMin
	offlineSince := time.Now()
	for ctx.Err() == nil {
		state.current.mu.Lock()
		closed := state.current.closed
		state.current.mu.Unlock()
		expired := time.Since(offlineSince) >= c.cfg.OfflineTimeout
		if closed || expired {
			if ctx.Err() != nil {
				break
			}
			mu.Lock()
			state.current.close()
			state.current = newSession(ctx, "", c.cfg.Options)
			mu.Unlock()
			id, resume = randomID(), false
			offlineSince = time.Now()
			backoff = c.cfg.ReconnectMin
			if expired {
				c.cfg.Options.Logf("offline timeout; existing TCP connections and replay buffer released")
			}
		}
		// A stalled HTTP/TLS handshake must not extend offline retention.
		dialCtx, cancelDial := context.WithDeadline(ctx, offlineSince.Add(c.cfg.OfflineTimeout))
		ws, crypt, status, err := c.dial(dialCtx, id, resume)
		cancelDial()
		if status == http.StatusGone && resume {
			// A server restart/expired session cannot preserve the backend TCP sockets.
			mu.Lock()
			state.current.close()
			state.current = newSession(ctx, "", c.cfg.Options)
			mu.Unlock()
			id = randomID()
			resume = false
			backoff = c.cfg.ReconnectMin
			offlineSince = time.Now()
			c.cfg.Options.Logf("server session lost; existing TCP connections closed")
			continue
		}
		if err == nil {
			resume = true
			backoff = c.cfg.ReconnectMin
			c.cfg.Options.Logf("tunnel connected")
			err = state.current.transport(ws, crypt)
			offlineSince = time.Now()
		}
		if ctx.Err() != nil {
			break
		}
		delay := backoff/2 + time.Duration(rand.Int64N(int64(backoff/2)+1))
		if remaining := time.Until(offlineSince.Add(c.cfg.OfflineTimeout)); delay > remaining {
			delay = max(remaining, 0)
		}
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
}
