package tunnel

import (
	"bytes"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"
)

// 用可控的应用写阻塞替代依赖操作系统缓冲区大小的偶发背压。
type pausedApplicationConn struct {
	net.Conn
	release chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (c *pausedApplicationConn) Write(p []byte) (int, error) {
	select {
	case <-c.release:
		return c.Conn.Write(p)
	case <-c.closed:
		return 0, net.ErrClosed
	}
}
func (c *pausedApplicationConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// 模拟视频预读后暂停读取TCP：旧实现会卡住WebSocket读者，噪音最终写超时。
// 两端缓存故意不一致，验证认证窗口能限制在途业务，给噪音/ACK/心跳留出读取能力。
func TestNoiseSurvivesApplicationBackpressure(t *testing.T) {
	for _, mode := range []string{"", "tls"} {
		t.Run("protocol="+mode, func(t *testing.T) {
			noiseRecoveryResourceCheck(t)
			serverStats, clientStats := NewStats(), NewStats()
			opts := Options{Stats: serverStats, BufferBytes: 128 * 1024, Heartbeat: 80 * time.Millisecond,
				HeartbeatTimeout: 600 * time.Millisecond, DialTimeout: 200 * time.Millisecond, WriteTimeout: 100 * time.Millisecond,
				NoiseIntervalMin: 10 * time.Millisecond, NoiseIntervalMax: 30 * time.Millisecond, NoiseMinBytes: 10240, NoiseMaxBytes: 102400,
				NoiseSuppressTraffic: true, NoiseIdleAfter: 50 * time.Millisecond, Logf: func(string, ...any) {}}
			hold := 2 * time.Second
			if os.Getenv("SGATE_PRODUCTION_BACKPRESSURE") == "1" {
				opts.Heartbeat, opts.HeartbeatTimeout, opts.DialTimeout, opts.WriteTimeout = 0, 0, 0, 0
				opts.NoiseIdleAfter = time.Second
				hold = 20 * time.Second
			}
			payload := make([]byte, 256*1024)
			for i := range payload {
				payload[i] = byte((i*17 + i/251) % 253)
			}
			f := newRecoveryFixtureBackend(t, 2*time.Minute, 2*time.Minute, mode, opts, func(c net.Conn) {
				var start [1]byte
				if _, err := io.ReadFull(c, start[:]); err != nil {
					return
				}
				if _, err := c.Write(payload); err != nil {
					return
				}
				io.Copy(c, c)
			}, func(c *ClientConfig) {
				c.Options.Stats = clientStats
				c.Options.BufferBytes = 2 * (chunkSize + frameHeader)
				c.Options.NoiseIntervalMin = 0
				c.Options.NoiseIntervalMax = 0
			})
			// 在接入会话前安装阻塞器，不替换已经被读协程使用的连接。
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			c, err := net.Dial("tcp", listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			accepted, err := listener.Accept()
			if err != nil {
				t.Fatal(err)
			}
			listener.Close()
			paused := &pausedApplicationConn{Conn: accepted, release: make(chan struct{}), closed: make(chan struct{})}
			defer paused.Close()
			c.SetDeadline(time.Now().Add(hold + 20*time.Second))
			client := f.clientSession()
			if err := client.addLocal(paused); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Write([]byte("s")); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "application backpressure and receive queue", func() bool {
				return clientStats.Snapshot().NoiseActivity.ReceiveBufferBytes == 2*(chunkSize+frameHeader)
			})
			remote := f.remoteSession()
			remote.mu.Lock()
			window := remote.peerWindow
			remote.mu.Unlock()
			if window != 2*(chunkSize+frameHeader) {
				t.Fatalf("peer receive window not negotiated: %d", window)
			}
			waitFor(t, "idle noise received while application blocked", func() bool {
				return serverStats.Snapshot().NoiseActivity.Idle == 1 && clientStats.Snapshot().NoiseRX > 0
			})
			before := clientStats.Snapshot()
			for step := 0; step < int(hold/(200*time.Millisecond)); step++ {
				time.Sleep(200 * time.Millisecond)
				v := clientStats.Snapshot()
				if v.NoiseActivity.Disconnects != 0 || v.NoiseActivity.Tunnels != 1 || v.NoiseActivity.Streams != 1 {
					t.Fatalf("slow application broke tunnel at %dms: %+v", (step+1)*200, v)
				}
				if v.NoiseRX <= before.NoiseRX {
					t.Fatalf("noise stalled behind business write: before=%+v now=%+v", before, v)
				}
				if v.NoiseActivity.ReceiveBufferBytes != 2*(chunkSize+frameHeader) {
					t.Fatal("unbounded receive cache", v)
				}
				before = v
			}
			t.Logf("paused %s with full receive queue, no disconnect: %+v", hold, before)
			f.disconnect()
			f.blocked.Store(false)
			waitFor(t, "noise resumes while application remains blocked", func() bool {
				v := clientStats.Snapshot()
				return v.NoiseRX > before.NoiseRX && v.NoiseActivity.Tunnels == 1 && f.remoteSession() == remote
			})
			close(paused.release)
			got := make([]byte, len(payload))
			if _, err := io.ReadFull(c, got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatal("replayed data duplicated or corrupted")
			}
			if err := exchange(c, []byte("after-paused-video")); err != nil {
				t.Fatal(err)
			}
			c.Close()
			waitFor(t, "business and both cache directions released", func() bool {
				for _, s := range []*session{client, remote} {
					s.mu.Lock()
					clean := len(s.streams) == 0 && s.pendingBytes == 0 && s.incomingBytes == 0
					s.mu.Unlock()
					if !clean {
						return false
					}
				}
				return true
			})
		})
	}
}
