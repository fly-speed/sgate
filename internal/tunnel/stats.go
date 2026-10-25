package tunnel

import (
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Stats counts actual tunnel TCP bytes, including HTTP, WS, TLS and padding.
// Application sockets and a separate management listener are not counted.
// With shared server management, bytes and connections on that HTTP/TLS
// listener also include management traffic.
type Stats struct {
	mu                     sync.Mutex
	noiseSessions          map[*session]int
	sessions               map[*session]struct{}
	disconnects            uint64
	lastDisconnect         string
	lastDisconnectTime     int64
	businessTX, businessRX atomic.Uint64
	tx, rx                 atomic.Uint64
	noiseTX, noiseRX       atomic.Uint64
	active                 atomic.Int64
	started                time.Time
}

// BusinessTX/RX 是实际加密帧中的业务负载，包含重放，不代表应用重复交付。
// NoiseActivity 是采样时刻的状态；Permitted 表示策略允许，不保证该秒有噪音帧。
type TrafficSnapshot struct {
	BusinessTX    uint64                `json:"business_tx_bytes"`
	BusinessRX    uint64                `json:"business_rx_bytes"`
	NoiseActivity NoiseActivitySnapshot `json:"noise_activity"`
	NoiseTX       uint64                `json:"noise_tx_bytes"`
	NoiseRX       uint64                `json:"noise_rx_bytes"`
	TX            uint64                `json:"tx_bytes"`
	RX            uint64                `json:"rx_bytes"`
	Connections   int64                 `json:"connections"`
	Time          int64                 `json:"time_ms"`
	Uptime        int64                 `json:"uptime_seconds"`
}

func NewStats() *Stats { return &Stats{started: time.Now()} }
func (s *Stats) Snapshot() TrafficSnapshot {
	return TrafficSnapshot{
		BusinessTX: s.businessTX.Load(), BusinessRX: s.businessRX.Load(), NoiseActivity: s.noiseActivity(),
		NoiseTX: s.noiseTX.Load(), NoiseRX: s.noiseRX.Load(),
		TX:          s.tx.Load(),
		RX:          s.rx.Load(),
		Connections: s.active.Load(),
		Time:        time.Now().UnixMilli(),
		Uptime:      int64(time.Since(s.started).Seconds()),
	}
}
func (s *Stats) Conn(c net.Conn) net.Conn {
	s.active.Add(1)
	return &trafficConn{Conn: c, stats: s}
}
func (s *Stats) Listener(l net.Listener) net.Listener { return &trafficListener{Listener: l, stats: s} }

type trafficConn struct {
	net.Conn
	stats *Stats
	once  sync.Once
}

func (c *trafficConn) Read(p []byte) (int, error) {
	n, e := c.Conn.Read(p)
	c.stats.rx.Add(uint64(n))
	return n, e
}
func (c *trafficConn) Write(p []byte) (int, error) {
	n, e := c.Conn.Write(p)
	c.stats.tx.Add(uint64(n))
	return n, e
}
func (c *trafficConn) Close() error {
	e := c.Conn.Close()
	c.once.Do(func() { c.stats.active.Add(-1) })
	return e
}

type trafficListener struct {
	net.Listener
	stats *Stats
}

func (l *trafficListener) Accept() (net.Conn, error) {
	c, e := l.Listener.Accept()
	if e != nil {
		return nil, e
	}
	return l.stats.Conn(c), nil
}
