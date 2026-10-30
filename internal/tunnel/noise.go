package tunnel

import (
	"crypto/rand"
	"math/big"
	"time"

	"github.com/gorilla/websocket"
)

// Each call independently chooses the next wait, including the initial wait
// after connecting. Equal bounds also support a fixed interval.
func (o Options) noiseDelay() (time.Duration, error) {
	if o.NoiseSuppressTraffic && o.NoiseSuppressIdle && o.NoiseSuppressEmpty {
		return 0, nil
	}
	min, max := o.NoiseIntervalMin, o.NoiseIntervalMax
	if min == 0 && max == 0 {
		return o.NoiseInterval, nil
	}
	if min == max {
		return min, nil
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(max-min)+1))
	if err != nil {
		return 0, err
	}
	return min + time.Duration(n.Int64()), nil
}

// Noise uses the transport's encryption counter, but no reliable sequence or
// replay buffer. A lost padding frame is never retained or retransmitted.
func (o Options) noiseSize() (int, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(o.NoiseMaxBytes-o.NoiseMinBytes+1)))
	if err != nil {
		return 0, err
	}
	return o.NoiseMinBytes + int(n.Int64()), nil
}

// 每块不超过业务帧大小。噪音总量再大也不分配整块大内存。
func (s *session) writeNoise(ws *websocket.Conn, crypt *codec, size int) error {
	data := make([]byte, size)
	// 每次发送都重新用密码学随机数填满负载；随机源失败时返回错误，不发送零值填充。
	if _, err := rand.Read(data); err != nil {
		return err
	}
	_ = ws.SetWriteDeadline(time.Now().Add(s.opts.WriteTimeout))
	if err := ws.WriteMessage(websocket.BinaryMessage, crypt.seal((frame{op: opNoise, data: data}).encode())); err != nil {
		return err
	}
	if s.opts.Stats != nil {
		s.opts.Stats.noiseTX.Add(uint64(size))
		s.opts.Stats.peerPayload(ws.NetConn(), "noiseTX", size)
	}
	s.mu.Lock()
	s.noiseSent++
	s.mu.Unlock()
	return nil
}
