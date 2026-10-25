package master_gin

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	master "github.com/acl-dev/go-service"
	"github.com/gin-gonic/gin"
)

func TestInitRunRestartHijackedConnectionsAndRollback(t *testing.T) {
	baseline := master.ConnCountCur()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service, e := Init("127.0.0.1:0", WithContext(ctx), WithManaged(false))
	if e != nil {
		t.Fatal(e)
	}
	defer service.Close()
	var accepted, closed atomic.Int64
	service.AcceptHandler = func(net.Conn) { accepted.Add(1) }
	service.CloseHandler = func(net.Conn) { closed.Add(1) }
	service.GET("/*path", gin.WrapH(service.Handler()))
	service.NoRoute(gin.WrapH(service.Handler()))
	proxy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _, e := w.(http.Hijacker).Hijack()
		if e != nil {
			return
		}
		defer c.Close()
		fmt.Fprint(c, ("HTTP/1.1 101 Switching Protocols\r\n" +
			"Connection: Upgrade\r\n" +
			"Upgrade: test\r\n" +
			"\r\n"))
		io.Copy(c, c)
	})
	worker, e := service.Configure(ServerConfig{Handler: proxy, ReadHeaderTimeout: time.Second})
	if e != nil {
		t.Fatal(e)
	}
	runDone := make(chan struct{})
	go func() {
		service.Run()
		close(runDone)
	}()
	address := service.Addresses()[0]
	conn, e := net.DialTimeout("tcp", address, time.Second)
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(4 * time.Second))
	fmt.Fprint(conn, "GET /stream HTTP/1.1\r\nHost: local\r\n\r\n")
	reader := bufio.NewReader(conn)
	for {
		line, e := reader.ReadString('\n')
		if e != nil {
			t.Fatal(e)
		}
		if line == "\r\n" {
			break
		}
	}
	if service.ActiveConnections() != 1 || master.ConnCountCur() != baseline+1 {
		t.Fatal("hijacked connection prematurely decremented")
	}
	conn.Write([]byte("unique-hijacked-record"))
	buf := make([]byte, len("unique-hijacked-record"))
	if _, e = io.ReadFull(reader, buf); e != nil || string(buf) != "unique-hijacked-record" {
		t.Fatal("hijacked bytes", e)
	}
	if e = worker.Close(); e != nil {
		t.Fatal(e)
	}
	if n, e := reader.Read(buf[:1]); n != 0 || e == nil {
		t.Fatal("hijacked transport retained")
	}
	if service.ActiveConnections() != 0 || master.ConnCountCur() != baseline {
		t.Fatal("connection count retained")
	}
	select {
	case <-service.Context().Done():
		t.Fatal("proxy restart stopped process")
	default:
	}
	busy, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer busy.Close()
	if _, e = service.Configure(ServerConfig{Addresses: busy.Addr().String()}); e == nil {
		t.Fatal("occupied address accepted")
	}
	worker, e = service.Configure(ServerConfig{
		Addresses: address,
		Handler:   http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "new-generation") }),
	})
	if e != nil {
		t.Fatal(e)
	}
	req, _ := http.NewRequest("GET", "http://"+address+"/new", nil)
	req.Close = true
	client := &http.Client{Timeout: 3 * time.Second}
	resp, e := client.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "new-generation" {
		t.Fatal("wrong generation")
	}
	cancel()
	select {
	case <-runDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not join")
	}
	if e = service.Err(); e != nil {
		t.Fatal(e)
	}
	if service.ActiveConnections() != 0 || accepted.Load() != closed.Load() {
		t.Fatal("callbacks/connection cleanup", accepted.Load(), closed.Load())
	}
	rebind, e := net.Listen("tcp", address)
	if e != nil {
		t.Fatal("port retained", e)
	}
	rebind.Close()
}
func TestExampleInitRouteRunLifecycle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service, e := Init("127.0.0.1:0", WithContext(ctx), WithManaged(false))
	if e != nil {
		t.Fatal(e)
	}
	defer service.Close()
	service.GET("/test", func(c *gin.Context) { c.String(200, "example route") })
	done := make(chan struct{})
	go func() {
		service.Run()
		close(done)
	}()
	resp, e := (&http.Client{Timeout: time.Second}).Get("http://" + service.Addresses()[0] + "/test")
	if e != nil {
		t.Fatal(e)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "example route") {
		t.Fatal("route")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run retained")
	}
}

func TestShutdownUnblocksHijackedWriterBeforeProtocolHook(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, e := Init("127.0.0.1:0", WithContext(ctx), WithManaged(false))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	s.GET("/*path", gin.WrapH(s.Handler()))
	entered, exited := make(chan struct{}), make(chan struct{})
	w, e := s.Configure(ServerConfig{Handler: http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		c, _, e := rw.(http.Hijacker).Hijack()
		if e != nil {
			return
		}
		defer c.Close()
		defer close(exited)
		close(entered)
		data := make([]byte, 64*1024)
		for {
			if _, e := c.Write(data); e != nil {
				return
			}
		}
	}), OnStop: func() { <-exited }})
	if e != nil {
		t.Fatal(e)
	}
	runDone := make(chan struct{})
	go func() {
		s.Run()
		close(runDone)
	}()
	conn, e := net.DialTimeout("tcp", s.Addresses()[0], time.Second)
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	fmt.Fprint(conn, "GET /blocked HTTP/1.1\r\nHost: test\r\n\r\n")
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("handler not entered")
	}
	closed := make(chan struct{})
	go func() {
		w.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("protocol hook blocked before physical transport close")
	}
	cancel()
	<-runDone
	if s.ActiveConnections() != 0 {
		t.Fatal("connection retained")
	}
}
