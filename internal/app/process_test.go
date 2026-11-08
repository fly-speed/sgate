package app

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"sgate/internal/cli"
)

func TestManagementChildProcess(t *testing.T) {
	if os.Getenv("SGATE_MANAGEMENT_TEST_CHILD") != "1" {
		return
	}
	role := os.Getenv("SGATE_MANAGEMENT_TEST_ROLE")
	var args []string
	if e := json.Unmarshal([]byte(os.Getenv("SGATE_MANAGEMENT_TEST_ARGS")), &args); e != nil {
		t.Fatal(e)
	}
	fs := appFlags(role)
	if e := cli.Parse(fs, args); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if e := Run(ctx, role, fs, args, fs.Lookup("admin-config").Value.String()); e != nil {
		t.Fatal(e)
	}
}
func temporaryAddress(t *testing.T) string {
	t.Helper()
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	a := l.Addr().String()
	l.Close()
	return a
}

func TestManagementRealProcessRestartAndExpiry(t *testing.T) {
	for _, role := range []string{"client", "server"} {
		t.Run(role, func(t *testing.T) {
			dir := t.TempDir()
			key := filepath.Join(dir, "key")
			os.WriteFile(key, []byte(hex.EncodeToString(make([]byte, 32))), 0600)
			business, management := temporaryAddress(t), temporaryAddress(t)
			appPath, adminPath := filepath.Join(dir, "app.conf"), filepath.Join(dir, "admin.conf")
			listenKey := "master_service"
			appText := listenKey + " = " + business + "\nkey-file = " + key + "\npassword = process-test-password\n"
			if role == "client" {
				appText += testClientMappingAt(t, business, "ws://127.0.0.1:1/tunnel")
			} else {
				appText += "target = 127.0.0.1:1\n"
			}
			os.WriteFile(appPath, []byte(appText), 0600)
			webDir, e := filepath.Abs("../../web/admin")
			if e != nil {
				t.Fatal(e)
			}
			webSource := webDir
			webDir = filepath.Join(dir, "web")
			if e = os.Mkdir(webDir, 0700); e != nil {
				t.Fatal(e)
			}
			for _, name := range []string{"index.html", "app.css", "i18n.js", "app.js"} {
				b, e := os.ReadFile(filepath.Join(webSource, name))
				if e != nil {
					t.Fatal(e)
				}
				if e = os.WriteFile(filepath.Join(webDir, name), b, 0600); e != nil {
					t.Fatal(e)
				}
			}
			os.WriteFile(
				adminPath,
				[]byte("web-dir = "+webDir+"\nlisten = "+management+("\n"+
					"users-file = users.json\n"+
					"session-dir = sessions\n"+
					"setup-token-file = setup.token\n"+
					"session-timeout = 8s\n")),
				0600,
			)
			binary, e := os.Executable()
			if e != nil {
				t.Fatal(e)
			}
			args, _ := json.Marshal([]string{"-config", appPath, "-admin-config", adminPath})
			var child *exec.Cmd
			var log *os.File
			stop := func(crash bool) {
				if child == nil {
					return
				}
				force := crash || runtime.GOOS == "windows" // Process.Signal cannot send Interrupt on Windows.
				if force {
					e = child.Process.Kill()
				} else {
					e = child.Process.Signal(os.Interrupt)
				}
				if e != nil {
					t.Error(e)
				}
				done := make(chan error, 1)
				go func() { done <- child.Wait() }()
				select {
				case err := <-done:
					if !force && err != nil {
						b, _ := os.ReadFile(log.Name())
						t.Errorf("child exit: %v %s", err, b)
					}
				case <-time.After(10 * time.Second):
					child.Process.Kill()
					<-done
					t.Error("child did not stop")
				}
				log.Close()
				child = nil
			}
			defer func() { stop(false) }()
			jar, _ := cookiejar.New(nil)
			transport := &http.Transport{MaxIdleConns: 4, IdleConnTimeout: time.Second}
			defer transport.CloseIdleConnections()
			client := &http.Client{Jar: jar, Transport: transport, Timeout: 10 * time.Second}
			base := "http://" + management
			call := func(method, path, body string) (int, map[string]any, error) {
				r, e := http.NewRequest(method, base+path, strings.NewReader(body))
				if e != nil {
					return 0, nil, e
				}
				r.Header.Set("Content-Type", "application/json")
				resp, e := client.Do(r)
				if e != nil {
					return 0, nil, e
				}
				defer resp.Body.Close()
				b, e := io.ReadAll(resp.Body)
				if e != nil {
					return resp.StatusCode, nil, e
				}
				var v map[string]any
				e = json.Unmarshal(b, &v)
				return resp.StatusCode, v, e
			}
			start := func() {
				t.Helper()
				log, e = os.CreateTemp(dir, "child-*.log")
				if e != nil {
					t.Fatal(e)
				}
				child = exec.Command(binary, "-test.run=^TestManagementChildProcess$")
				child.Env = append(
					os.Environ(),
					"SGATE_MANAGEMENT_TEST_CHILD=1",
					"SGATE_MANAGEMENT_TEST_ROLE="+role,
					"SGATE_MANAGEMENT_TEST_ARGS="+string(args),
				)
				child.Stdout, child.Stderr = log, log
				if e := child.Start(); e != nil {
					t.Fatal(e)
				}
				waitManaged(
					t,
					"management startup",
					func() bool {
						code, _, e := call("GET", "/api/status", "")
						return e == nil && code == 200
					},
				)
			}
			start()
			indexPath := filepath.Join(webDir, "index.html")
			index, e := os.ReadFile(indexPath)
			if e != nil {
				t.Fatal(e)
			}
			for version := 0; version < 2; version++ {
				if version == 1 {
					index = append(index, []byte("\n<!-- external runtime edit -->")...)
					if e = os.WriteFile(indexPath, index, 0600); e != nil {
						t.Fatal(e)
					}
				}
				resp, e := client.Get(base + "/")
				if e != nil {
					t.Fatal(e)
				}
				body, e := io.ReadAll(resp.Body)
				resp.Body.Close()
				if e != nil || resp.StatusCode != 200 || string(body) != string(index) {
					t.Fatalf("external HTML version %d not served by live process", version)
				}
			}
			token, e := os.ReadFile(filepath.Join(dir, "setup.token"))
			if e != nil {
				t.Fatal(e)
			}
			payload, _ := json.Marshal(map[string]string{
				"username":    "process-admin",
				"password":    "process-management-password",
				"setup_token": strings.TrimSpace(string(token)),
			})
			code, v, e := call("POST", "/api/setup", string(payload))
			if e != nil || code != 200 {
				t.Fatalf("setup %d %v %v", code, e, v)
			}
			// Two graceful restarts and one abrupt exit use the exact same cookie jar.
			for i := 0; i < 3; i++ {
				stop(i == 2)
				start()
				code, v, e = call("GET", "/api/status", "")
				if e != nil || code != 200 || v["authenticated"] != true {
					t.Fatalf("restart %d lost login: %d %v %v", i, code, e, v)
				}
			}
			deadline := time.Now().Add(11 * time.Second)
			for {
				code, v, e = call("GET", "/api/stats", "")
				entries, _ := os.ReadDir(filepath.Join(dir, "sessions"))
				if e == nil && code == 401 && len(entries) == 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("expiry/cleanup: %d %v files=%d", code, e, len(entries))
				}
				time.Sleep(50 * time.Millisecond)
			}
			stop(false)
			start()
			code, v, e = call("GET", "/api/status", "")
			if e != nil || code != 200 || v["authenticated"] != false {
				t.Fatalf("expired login resurrected: %d %v %v", code, e, v)
			}
			login, _ := json.Marshal(map[string]string{"username": "process-admin", "password": "process-management-password"})
			code, _, e = call("POST", "/api/login", string(login))
			if e != nil || code != 200 {
				t.Fatalf("fresh login failed: %d %v", code, e)
			}
			stop(false)
			for _, address := range []string{business, management} {
				l, e := net.Listen("tcp", address)
				if e != nil {
					t.Fatalf("listener retained: %s %v", address, e)
				}
				l.Close()
			}
			t.Log(fmt.Sprintf("%s: 3 restarts (including kill), expiry, fresh login, both listeners released", role))
		})
	}
}
