package app

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Run against production executables, including the legacy path without Web
// management. TestManagementDeploymentBinaries covers configuration + Web UI.
func TestStandaloneStartModesBinaries(t *testing.T) {
	cb, sb := os.Getenv("SGATE_ADMIN_CLIENT_BINARY"), os.Getenv("SGATE_ADMIN_SERVER_BINARY")
	if cb == "" || sb == "" {
		t.Skip("set SGATE_ADMIN_CLIENT_BINARY and SGATE_ADMIN_SERVER_BINARY")
	}
	for _, secure := range []bool{false, true} {
		for _, mode := range []string{"implicit-cli", "alone-cli", "alone-config", "alone-merged", "f-merged"} {
			t.Run(fmt.Sprintf("tls=%t/%s", secure, mode), func(t *testing.T) {
				dir := t.TempDir()
				key := filepath.Join(dir, "test.key")
				deploymentWrite(t, key, []byte(strings.Repeat("47", 32)))
				backend, e := net.Listen("tcp", "127.0.0.1:0")
				if e != nil {
					t.Fatal(e)
				}
				var peers sync.WaitGroup
				var conns sync.Map
				acceptDone := make(chan struct{})
				go func() {
					defer close(acceptDone)
					for {
						c, e := backend.Accept()
						if e != nil {
							return
						}
						conns.Store(c, true)
						peers.Add(1)
						go func() {
							defer peers.Done()
							defer conns.Delete(c)
							defer c.Close()
							io.Copy(c, c)
						}()
					}
				}()
				t.Cleanup(func() {
					backend.Close()
					<-acceptDone
					conns.Range(func(k, v any) bool {
						k.(net.Conn).Close()
						return true
					})
					peers.Wait()
				})
				cert, tlsKey := "", ""
				scheme := "ws"
				if secure {
					cert, tlsKey = managedCertificate(t)
					scheme = "wss"
				}
				addresses := map[string]string{"server": temporaryAddress(t), "client": temporaryAddress(t)}
				type child struct {
					cmd     *exec.Cmd
					done    chan struct{}
					err     error
					logPath string
				}
				children := map[string]*child{}
				stop := func(role string) {
					c := children[role]
					if c == nil {
						return
					}
					select {
					case <-c.done:
					default:
						c.cmd.Process.Signal(os.Interrupt)
					}
					select {
					case <-c.done:
					case <-time.After(10 * time.Second):
						c.cmd.Process.Kill()
						<-c.done
						t.Error("standalone process did not stop", role)
					}
					if c.err != nil {
						b, _ := os.ReadFile(c.logPath)
						t.Errorf("standalone %s exit %v: %s", role, c.err, b)
					}
					delete(children, role)
				}
				for _, role := range []string{"server", "client"} {
					binary := sb
					if role == "client" {
						binary = cb
					}
					binary, e = filepath.Abs(binary)
					if e != nil {
						t.Fatal(e)
					}
					args := []string{
						"-listen",
						addresses[role],
						"-key-file",
						key,
						"-password",
						"standalone-test-password",
						"-max-streams",
						"64",
					}
					if role == "server" {
						args = append(args, "-target", backend.Addr().String())
						if secure {
							args = append(args, "-tls-cert", cert, "-tls-key", tlsKey)
						}
					} else {
						args = append(args, "-server", scheme+"://"+addresses["server"]+"/tunnel")
						if secure {
							args = append(args, "-tls-ca", cert)
						}
					}
					if mode == "alone-config" || strings.HasSuffix(mode, "merged") {
						// Explicit CLI listen/max-streams override the file. Running 16 streams
						// catches accidentally using the file's limit of one stream.
						text := "listen = 127.0.0.1:1\nmax-streams = 1\n"
						for i := 2; i < len(args); i += 2 {
							if args[i] == "-max-streams" {
								continue
							}
							text += strings.TrimPrefix(args[i], "-") + " = " + strconv.Quote(args[i+1]) + "\n"
						}
						if strings.HasSuffix(mode, "merged") {
							text = "service standalone {\n master_service = 127.0.0.1|1\n master_args =\n" + text + "}\n"
						}
						config := filepath.Join(dir, role+".cf")
						deploymentWrite(t, config, []byte(text))
						option := "-config"
						if mode == "f-merged" {
							option = "-f"
						}
						args = []string{option, config, "-listen", addresses[role], "-max-streams", "64"}
					}
					if mode != "implicit-cli" {
						args = append(args, "-alone")
					}
					logPath := filepath.Join(dir, role+".log")
					f, e := os.Create(logPath)
					if e != nil {
						t.Fatal(e)
					}
					c := &child{cmd: exec.Command(binary, args...), done: make(chan struct{}), logPath: logPath}
					c.cmd.Dir = dir
					c.cmd.Stdout, c.cmd.Stderr = f, f
					if e = c.cmd.Start(); e != nil {
						f.Close()
						t.Fatal(e)
					}
					children[role] = c
					go func() {
						c.err = c.cmd.Wait()
						f.Close()
						close(c.done)
					}()
					t.Cleanup(func() { stop(role) })
				}
				exchange := func(index int) error {
					c, e := net.DialTimeout("tcp", addresses["client"], time.Second)
					if e != nil {
						return e
					}
					defer c.Close()
					c.SetDeadline(time.Now().Add(5 * time.Second))
					payload := bytes.Repeat([]byte(fmt.Sprintf("unique-stream-%03d|", index)), 2048)
					if _, e = c.Write(payload); e != nil {
						return e
					}
					if e = c.(*net.TCPConn).CloseWrite(); e != nil {
						return e
					}
					got, e := io.ReadAll(c)
					if e != nil {
						return e
					}
					if !bytes.Equal(got, payload) {
						return fmt.Errorf("stream %d got %d want %d", index, len(got), len(payload))
					}
					return nil
				}
				waitManaged(t, "standalone tunnel", func() bool { return exchange(99) == nil })
				const count = 16
				var wg sync.WaitGroup
				errs := make(chan error, count)
				start := make(chan struct{})
				for i := 0; i < count; i++ {
					wg.Add(1)
					go func(i int) {
						defer wg.Done()
						<-start
						errs <- exchange(i)
					}(i)
				}
				close(start)
				wg.Wait()
				close(errs)
				for e := range errs {
					if e != nil {
						t.Fatal(e)
					}
				}
				stop("client")
				stop("server")
				for _, address := range addresses {
					l, e := net.Listen("tcp", address)
					if e != nil {
						t.Fatal("standalone listener retained", address, e)
					}
					l.Close()
				}
				t.Log(("both roles: standalone startup, unique duplex bytes, SIGINT shutdown and listener " +
					"release passed"))
			})
		}
	}
}
