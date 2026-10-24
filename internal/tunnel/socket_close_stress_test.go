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

// No tunnel, TLS, WebSocket, relay or session: isolate kernel socket shutdown
// under full receive queues from protocol-level resource cleanup.
func TestStressDirectTCPShutdown(t *testing.T) {
	if os.Getenv("SGATE_STRESS_SOCKET_CLOSE") != "1" {
		t.Skip("opt-in TCP shutdown control")
	}
	n := stressInt(t, "SGATE_STRESS_SOCKET_CLOSE_COUNT", 1024)
	size := stressInt(t, "SGATE_STRESS_SOCKET_CLOSE_BUFFER", 65536)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var servers, peers []net.Conn
	var mu sync.Mutex
	var wg sync.WaitGroup
	cleanup := func() {
		l.Close()
		mu.Lock()
		for _, c := range servers {
			c.Close()
		}
		for _, c := range peers {
			c.Close()
		}
		mu.Unlock()
		wg.Wait()
	}
	defer cleanup()
	accepted := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			c, e := l.Accept()
			if e != nil {
				accepted <- e
				return
			}
			if e = stressSocketBuffer(c, size); e != nil {
				c.Close()
				accepted <- e
				return
			}
			mu.Lock()
			servers = append(servers, c)
			mu.Unlock()
		}
		accepted <- nil
	}()
	result := make(chan error, n)
	gate := make(chan struct{})
	var gateOnce sync.Once
	defer gateOnce.Do(func() { close(gate) })
	payload := make([]byte, 512*1024)
	for i := 0; i < n; i++ {
		c, e := net.DialTimeout("tcp", l.Addr().String(), 5*time.Second)
		if e != nil {
			t.Fatal(e)
		}
		if e = stressSocketBuffer(c, size); e != nil {
			c.Close()
			t.Fatal(e)
		}
		mu.Lock()
		peers = append(peers, c)
		mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer c.Close()
			sent := make(chan struct{})
			go func() { defer close(sent); <-gate; _, _ = io.Copy(c, bytes.NewReader(payload)) }()
			var b [1]byte
			_, e := c.Read(b[:])
			result <- e
			c.Close()
			<-sent
		}()
	}
	if err := <-accepted; err != nil {
		t.Fatal(err)
	}
	if path := os.Getenv("SGATE_STRESS_SOCKET_CLOSE_READY"); path != "" {
		if err := os.WriteFile(path, []byte("ready"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if path := os.Getenv("SGATE_STRESS_SOCKET_CLOSE_GATE"); path != "" {
		until := time.Now().Add(15 * time.Second)
		for {
			if _, err := os.Stat(path); err == nil {
				break
			}
			if time.Now().After(until) {
				t.Fatal("global TCP fill barrier timed out")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	gateOnce.Do(func() { close(gate) })
	time.Sleep(2 * time.Second)
	started := time.Now()
	mu.Lock()
	for _, c := range servers {
		c.Close()
	}
	mu.Unlock()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	received := 0
	for received < n {
		select {
		case err := <-result:
			if err == nil {
				t.Error("unexpected successful read")
			}
			received++
		case <-timer.C:
			t.Fatalf("only %d/%d raw TCP peers observed close in %s", received, n, time.Since(started))
		}
	}
	t.Logf("all %d raw TCP peers observed close in %s", n, time.Since(started))
}
