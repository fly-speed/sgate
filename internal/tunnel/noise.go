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
func (s *session) writeNoise(ws *websocket.Conn, crypt *codec) error {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(s.opts.NoiseMaxBytes-s.opts.NoiseMinBytes+1)))
	if err != nil {
		return err
	}
	data := make([]byte, s.opts.NoiseMinBytes+int(n.Int64()))
	if _, err := rand.Read(data); err != nil {
		return err
	}
	_ = ws.SetWriteDeadline(time.Now().Add(s.opts.WriteTimeout))
	if err := ws.WriteMessage(websocket.BinaryMessage, crypt.seal((frame{op: opNoise, data: data}).encode())); err != nil {
		return err
	}
	s.mu.Lock()
	s.noiseSent++
	s.mu.Unlock()
	return nil
}
