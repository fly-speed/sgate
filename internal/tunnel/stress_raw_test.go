package tunnel

import (
	"encoding/binary"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// An opt-in direct TCP control separates tunnel failures from load-generator or
// host networking failures, using exactly the same independent stream verifier.
func TestStressRawDuplexBaseline(t *testing.T) {
	if os.Getenv("SGATE_STRESS_RAW") != "1" {
		t.Skip("set SGATE_STRESS_RAW=1 for direct TCP control")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	const streams, size = 256, 128 * 1024
	end := time.Now().Add(10 * time.Second)
	var upSent, upReceived, downSent, downReceived atomic.Int64
	results := make(chan error, streams*2)
	var wg sync.WaitGroup
	var connections sync.Map
	t.Cleanup(func() {
		l.Close()
		connections.Range(func(c, _ any) bool {
			c.(net.Conn).Close()
			return true
		})
		wg.Wait()
	})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < streams; i++ {
			c, err := l.Accept()
			if err != nil {
				results <- err
				return
			}
			connections.Store(c, true)
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer c.Close()
				defer connections.Delete(c)
				c.SetDeadline(end.Add(20 * time.Second))
				var seed [8]byte
				if _, err := io.ReadFull(c, seed[:]); err != nil {
					results <- err
					return
				}
				results <- stressDuplex(
					c,
					binary.BigEndian.Uint64(seed[:]),
					end,
					size,
					true,
					&downSent,
					&upReceived,
					250*time.Millisecond,
				)
			}()
		}
	}()
	for i := 0; i < streams; i++ {
		c, err := net.Dial("tcp", l.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		connections.Store(c, true)
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer c.Close()
			defer connections.Delete(c)
			c.SetDeadline(end.Add(20 * time.Second))
			var seed [8]byte
			binary.BigEndian.PutUint64(seed[:], uint64(i))
			if _, err := c.Write(seed[:]); err != nil {
				results <- err
				return
			}
			results <- stressDuplex(c, uint64(i), end, size, false, &upSent, &downReceived, 250*time.Millisecond)
		}(i)
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Error(err)
		}
	}
	if upSent.Load() != upReceived.Load() || downSent.Load() != downReceived.Load() {
		t.Fatal("direct TCP byte counts differ")
	}
	t.Logf("direct TCP verified upload=%d download=%d", upReceived.Load(), downReceived.Load())
}
