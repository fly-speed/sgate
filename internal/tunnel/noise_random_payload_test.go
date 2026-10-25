package tunnel

import (
	"bytes"
	"fmt"
	"testing"
	"time"
)

// 检查真实线路解密后的负载，避免仅凭密文不同就误判明文也是随机数据。
// 这些检查用于捕获零值或固定填充回归，随机性的保证来自生产代码的 crypto/rand。
func TestNoiseRandomPayloadOnWire(t *testing.T) {
	for _, tlsMode := range []bool{false, true} {
		for _, clientWriter := range []bool{false, true} {
			name := fmt.Sprintf("tls=%t/client-writer=%t", tlsMode, clientWriter)
			t.Run(name, func(t *testing.T) {
				// 固定总大小，且每次拆成两块，确认各块都重新生成随机内容。
				opts := Options{
					NoiseInterval: 5 * time.Millisecond,
					NoiseMinBytes: 2 * chunkSize,
					NoiseMaxBytes: 2 * chunkSize,
				}
				_, peer, rx := noiseWirePair(t, tlsMode, clientWriter, opts)
				if err := peer.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
					t.Fatal(err)
				}
				seen := make(map[string]bool)
				for len(seen) < 8 {
					_, sealed, err := peer.ReadMessage()
					if err != nil {
						t.Fatal(err)
					}
					plain, err := rx.open(sealed)
					if err != nil {
						t.Fatal(err)
					}
					f, err := decodeFrame(plain)
					if err != nil {
						t.Fatal(err)
					}
					if f.op == opAck {
						continue
					}
					if f.op != opNoise || len(f.data) != chunkSize {
						t.Fatalf("unexpected noise frame: op=%d size=%d", f.op, len(f.data))
					}
					// 允许随机数据中出现 00，只禁止整块都为同一个字节。
					if bytes.Count(f.data, f.data[:1]) == len(f.data) {
						t.Fatalf("noise payload filled with constant byte %02x", f.data[0])
					}
					payload := string(f.data)
					if seen[payload] {
						t.Fatal("noise payload reused across blocks or bursts")
					}
					seen[payload] = true
				}
			})
		}
	}
}
