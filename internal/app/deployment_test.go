package app

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Opt-in: run installed production binaries from unrelated working directories.
// Both ordinary and race-instrumented binaries can use the same deployment tree.
func TestManagementDeploymentBinaries(t *testing.T) {
	cb, sb := os.Getenv("SGATE_ADMIN_CLIENT_BINARY"), os.Getenv("SGATE_ADMIN_SERVER_BINARY")
	if cb == "" || sb == "" {
		t.Skip("set SGATE_ADMIN_CLIENT_BINARY and SGATE_ADMIN_SERVER_BINARY")
	}
	var e error
	cb, e = filepath.Abs(cb)
	if e != nil {
		t.Fatal(e)
	}
	sb, e = filepath.Abs(sb)
	if e != nil {
		t.Fatal(e)
	}
	for _, secure := range []bool{false, true} {
		for _, mode := range []string{"default", "relative", "absolute"} {
			t.Run(
				fmt.Sprintf("https=%t/web=%s", secure, mode),
				func(t *testing.T) { runManagementDeployment(t, cb, sb, secure, mode) },
			)
		}
	}
}

type deployedAdmin struct {
	t                                                                                           *testing.T
	role, binary, appPath, adminPath, webDir, sessionDir, usersPath, management, business, base string
	cwds                                                                                        []string
	cmd                                                                                         *exec.Cmd
	done                                                                                        chan struct{}
	exit                                                                                        error
	logPath                                                                                     string
	client                                                                                      *http.Client
	lastCookies                                                                                 []*http.Cookie
	csrf                                                                                        string
	launches                                                                                    int
}

func deploymentWrite(t *testing.T, path string, b []byte) {
	t.Helper()
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(path, b, 0600); e != nil {
		t.Fatal(e)
	}
}
func (d *deployedAdmin) spawn(cwd string, relative bool) {
	d.t.Helper()
	if d.cmd != nil {
		d.t.Fatal("previous child not stopped")
	}
	app, admin := d.appPath, d.adminPath
	if relative {
		var e error
		app, e = filepath.Rel(cwd, app)
		if e != nil {
			d.t.Fatal(e)
		}
		admin, e = filepath.Rel(cwd, admin)
		if e != nil {
			d.t.Fatal(e)
		}
	}
	d.launches++
	d.logPath = filepath.Join(filepath.Dir(d.appPath), fmt.Sprintf("%s-%d.log", d.role, d.launches))
	f, e := os.Create(d.logPath)
	if e != nil {
		d.t.Fatal(e)
	}
	d.cmd = exec.Command(d.binary, "-config", app, "-admin-config", admin)
	d.cmd.Dir = cwd
	d.cmd.Stdout, d.cmd.Stderr = f, f
	if e = d.cmd.Start(); e != nil {
		f.Close()
		d.cmd = nil
		d.t.Fatal(e)
	}
	d.done = make(chan struct{})
	go func() {
		d.exit = d.cmd.Wait()
		f.Close()
		close(d.done)
	}()
}
func (d *deployedAdmin) start(cwd string, relative bool) {
	d.t.Helper()
	d.spawn(cwd, relative)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-d.done:
			b, _ := os.ReadFile(d.logPath)
			d.cmd = nil
			d.t.Fatalf("%s startup: %s", d.role, b)
		default:
		}
		code, _, e := d.raw("GET", "/api/status", nil)
		if e == nil && code == 200 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	d.t.Fatal("management startup timeout", d.role)
}
func (d *deployedAdmin) stop() {
	d.t.Helper()
	if d.cmd == nil {
		return
	}
	select {
	case <-d.done:
	default:
		if e := d.cmd.Process.Signal(os.Interrupt); e != nil {
			d.t.Error(e)
		}
	}
	select {
	case <-d.done:
	case <-time.After(12 * time.Second):
		d.cmd.Process.Kill()
		<-d.done
		d.t.Error("process shutdown timed out")
	}
	if d.exit != nil {
		b, _ := os.ReadFile(d.logPath)
		d.t.Errorf("%s shutdown %v: %s", d.role, d.exit, b)
	}
	d.cmd = nil
}
func (d *deployedAdmin) failStart(cwd string, want string) {
	d.t.Helper()
	d.spawn(cwd, true)
	select {
	case <-d.done:
	case <-time.After(10 * time.Second):
		d.cmd.Process.Kill()
		<-d.done
		d.cmd = nil
		d.t.Fatal("bad deployment did not fail promptly")
	}
	b, _ := os.ReadFile(d.logPath)
	d.cmd = nil
	if d.exit == nil || !strings.Contains(string(b), want) ||
		strings.Contains(string(b), "WARNING: DATA RACE") {
		d.t.Fatalf("%s bad deployment: exit=%v expected %q: %s", d.role, d.exit, want, b)
	}
	d.assertPortsReleased()
}
func (d *deployedAdmin) assertPortsReleased() {
	d.t.Helper()
	for _, address := range []string{d.business, d.management} {
		l, e := net.Listen("tcp", address)
		if e != nil {
			d.t.Fatalf("%s port retained %s: %v", d.role, address, e)
		}
		l.Close()
	}
}
func (d *deployedAdmin) raw(method, path string, input any) (int, []byte, error) {
	var b []byte
	if input != nil {
		b, _ = json.Marshal(input)
	}
	r, e := http.NewRequest(method, d.base+path, bytes.NewReader(b))
	if e != nil {
		return 0, nil, e
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-CSRF-Token", d.csrf)
	resp, e := d.client.Do(r)
	if e != nil {
		return 0, nil, e
	}
	defer resp.Body.Close()
	d.lastCookies = resp.Cookies()
	b, e = io.ReadAll(resp.Body)
	return resp.StatusCode, b, e
}
func (d *deployedAdmin) api(method, path string, input any, want int) map[string]any {
	d.t.Helper()
	code, b, e := d.raw(method, "/api/"+path, input)
	if e != nil || code != want {
		d.t.Fatalf("%s %s %s: %d want %d err=%v body=%s", d.role, method, path, code, want, e, b)
	}
	var v map[string]any
	if e = json.Unmarshal(b, &v); e != nil {
		d.t.Fatal(e)
	}
	return v
}
func (d *deployedAdmin) auth() {
	d.t.Helper()
	v := d.api("GET", "status", nil, 200)
	if v["authenticated"] != true {
		d.t.Fatal("session lost", d.role, v)
	}
	d.csrf = v["csrf"].(string)
}
func (d *deployedAdmin) asset(path string, want []byte, status int) {
	d.t.Helper()
	code, b, e := d.raw("GET", path, nil)
	if e != nil || code != status || (want != nil && !bytes.Equal(want, b)) {
		d.t.Fatalf("%s asset %s: %d want %d err=%v", d.role, path, code, status, e)
	}
}
func (d *deployedAdmin) save(want int) {
	d.t.Helper()
	v := d.api("GET", "config", nil, 200)
	d.api(
		"PUT",
		"config",
		map[string]any{
			"content":  v["content"].(string) + "\n# deployment save/restart\n",
			"revision": v["revision"],
			"apply":    true,
		},
		want,
	)
}

func runManagementDeployment(t *testing.T, cb, sb string, secure bool, mode string) {
	root := t.TempDir()
	release := filepath.Join(root, "release with spaces")
	conf := filepath.Join(release, "conf")
	work := filepath.Join(root, "unrelated working directories")
	cwds := []string{
		filepath.Join(work, "client cwd"),
		filepath.Join(work, "server cwd"),
		filepath.Join(root, "restart cwd"),
	}
	for _, cwd := range cwds {
		if e := os.MkdirAll(cwd, 0700); e != nil {
			t.Fatal(e)
		}
	}
	// Deliberate cwd decoys catch accidental resolution outside the config directory.
	for _, cwd := range cwds {
		for _, name := range []string{"index.html", "app.css", "i18n.js", "app.js"} {
			deploymentWrite(t, filepath.Join(cwd, "web/admin", name), []byte("wrong cwd"))
		}
	}
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
	key := filepath.Join(release, "keys", "sgate.key")
	deploymentWrite(t, key, []byte(hex.EncodeToString(bytes.Repeat([]byte{73}, 32))))
	cert, tlsKey := "", ""
	roots := x509.NewCertPool()
	if secure {
		cert, tlsKey = managedCertificate(t)
		b, _ := os.ReadFile(cert)
		if !roots.AppendCertsFromPEM(b) {
			t.Fatal("test CA invalid")
		}
	}
	scheme, wire := "http", "ws"
	if secure {
		scheme, wire = "https", "wss"
	}
	serverAddress, clientAddress := temporaryAddress(t), temporaryAddress(t)
	nodes := make([]*deployedAdmin, 0, 2)
	for _, role := range []string{"server", "client"} {
		business, binary, cwd := serverAddress, sb, cwds[1]
		if role == "client" {
			business, binary, cwd = clientAddress, cb, cwds[0]
		}
		roleConf := filepath.Join(conf, role)
		// Default ../web/admin is relative to the directory containing admin.conf.
		webDir := filepath.Join(conf, "web", "admin")
		webSetting := ""
		if mode != "default" {
			webDir = filepath.Join(release, role+" external UI")
			value := webDir
			if mode == "relative" {
				value, e = filepath.Rel(roleConf, webDir)
				if e != nil {
					t.Fatal(e)
				}
			}
			webSetting = "web-dir = " + strconv.Quote(value) + "\n"
		}
		for _, name := range []string{"index.html", "app.css", "i18n.js", "app.js"} {
			source, e := os.ReadFile(filepath.Join("../../web/admin", name))
			if e != nil {
				t.Fatal(e)
			}
			deploymentWrite(t, filepath.Join(webDir, name), source)
		}
		d := &deployedAdmin{
			t:          t,
			role:       role,
			binary:     binary,
			business:   business,
			management: temporaryAddress(t),
			appPath:    filepath.Join(roleConf, "app.conf"),
			adminPath:  filepath.Join(roleConf, "admin.conf"),
			webDir:     webDir,
			usersPath:  filepath.Join(roleConf, "users.json"),
			sessionDir: filepath.Join(roleConf, "sessions"),
			cwds:       cwds,
		}
		jar, _ := cookiejar.New(nil)
		transport := &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
			IdleConnTimeout: time.Second,
		}
		d.client = &http.Client{Jar: jar, Transport: transport, Timeout: 10 * time.Second}
		t.Cleanup(transport.CloseIdleConnections)
		t.Cleanup(d.stop)
		t.Cleanup(func() {
			os.Chmod(filepath.Dir(d.appPath), 0700)
			os.Chmod(d.webDir, 0700)
			for _, name := range []string{"index.html", "app.css", "i18n.js", "app.js"} {
				os.Chmod(filepath.Join(d.webDir, name), 0600)
			}
		})
		d.base = scheme + "://" + d.management
		if role == "server" {
			d.base = scheme + "://" + business + "/admin"
		} else {
			// 客户端也经 /admin 完成登录、保存、应用及进程重启回归。
			d.base += "/admin"
		}
		listenKey := "master_service"
		common := listenKey + " = " + business + "\nkey-file = " + strconv.Quote(key) + ("\n" +
			"password = deployment-test-password\n" +
			"noise-interval-min = 20ms\n" +
			"noise-interval-max = 40ms\n" +
			"noise-min-bytes = 128\n" +
			"noise-max-bytes = 512\n")
		if role == "server" {
			common += "target = " + backend.Addr().String() + "\nsession-ttl = 300ms\n"
			if secure {
				common += "tls-cert = " + strconv.Quote(cert) + "\ntls-key = " + strconv.Quote(tlsKey) + "\n"
			}
		} else {
			common += testClientMappingAt(t, business, wire+"://"+serverAddress+"/tunnel") + "reconnect-min = 10ms\nreconnect-max = 30ms\n"
			if secure {
				common += "tls-ca = " + strconv.Quote(cert) + "\n"
			}
		}
		deploymentWrite(t, d.appPath, []byte(common))
		adminText := webSetting + "listen = " + d.management + ("\n" +
			"users-file = users.json\n" +
			"session-dir = sessions\n" +
			"setup-token-file = setup.token\n" +
			"session-timeout = 1h\n")
		if secure {
			relativeCert, e := filepath.Rel(roleConf, cert)
			if e != nil {
				t.Fatal(e)
			}
			relativeKey, e := filepath.Rel(roleConf, tlsKey)
			if e != nil {
				t.Fatal(e)
			}
			adminText += "tls-cert = " + strconv.Quote(relativeCert) + "\ntls-key = " + strconv.Quote(relativeKey) + "\n"
		}
		deploymentWrite(t, d.adminPath, []byte(adminText))
		d.start(cwd, true)
		if role == "server" {
			probe, err := net.Listen("tcp", d.management)
			if err != nil {
				t.Fatalf("legacy management port still bound: %v", err)
			}
			probe.Close()
			d.api("GET", "config", nil, 401)
		}
		token, e := os.ReadFile(filepath.Join(roleConf, "setup.token"))
		if e != nil {
			t.Fatal(e)
		}
		v := d.api(
			"POST",
			"setup",
			map[string]string{
				"username":    "deploy-admin",
				"password":    "deployment-management-password",
				"setup_token": strings.TrimSpace(string(token)),
			},
			200,
		)
		d.csrf = v["csrf"].(string)
		if len(d.lastCookies) != 1 || d.lastCookies[0].Secure != secure || !d.lastCookies[0].HttpOnly ||
			d.lastCookies[0].SameSite != http.SameSiteStrictMode {
			t.Fatal("invalid deployed session cookie", role)
		}
		d.auth()
		req, _ := http.NewRequest("GET", d.base+"/api/status", nil)
		cookieFound := false
		for _, cookie := range d.client.Jar.Cookies(req.URL) {
			if strings.HasPrefix(cookie.Name, "sgate_admin_") {
				cookieFound = true
			}
		}
		if !cookieFound {
			t.Fatal("persistent cookie missing", role)
		}
		nodes = append(nodes, d)
	}
	exchange := func() {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		var last error
		for time.Now().Before(deadline) {
			c, e := net.DialTimeout("tcp", clientAddress, time.Second)
			if e != nil {
				last = e
				time.Sleep(20 * time.Millisecond)
				continue
			}
			c.SetDeadline(time.Now().Add(time.Second))
			payload := bytes.Repeat([]byte("deployment-byte-integrity"), 2048)
			_, e = c.Write(payload)
			got := make([]byte, len(payload))
			if e == nil {
				_, e = io.ReadFull(c, got)
			}
			c.Close()
			if e == nil {
				if !bytes.Equal(got, payload) {
					t.Fatal("deployment stream corrupt")
				}
				return
			}
			last = e
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("fresh stream did not recover", last)
	}
	exchange()
	permissionChecks := os.Geteuid() != 0
	for _, d := range nodes {
		// Atomic live replacement of each asset, without touching either process.
		for route, name := range map[string]string{
			"/":               "index.html",
			"/assets/app.css": "app.css",
			"/assets/app.js":  "app.js",
			"/assets/i18n.js": "i18n.js",
		} {
			path := filepath.Join(d.webDir, name)
			original, e := os.ReadFile(path)
			if e != nil {
				t.Fatal(e)
			}
			d.asset(route, original, 200)
			updated := append(append([]byte(nil), original...), []byte("\n/* external deployment hot update */")...)
			if e := writeDeploymentAtomic(path, updated); e != nil {
				t.Fatal(e)
			}
			d.asset(route, updated, 200)
			d.auth()
			if e = os.Rename(path, path+".missing"); e != nil {
				t.Fatal(e)
			}
			d.asset(route, nil, 503)
			d.auth()
			if e = os.Rename(path+".missing", path); e != nil {
				t.Fatal(e)
			}
			d.asset(route, updated, 200)
			if permissionChecks {
				if e = os.Chmod(path, 0); e != nil {
					t.Fatal(e)
				}
				d.asset(route, nil, 503)
				d.auth()
				if e = os.Chmod(path, 0444); e != nil {
					t.Fatal(e)
				}
				d.asset(route, updated, 200)
				os.Chmod(path, 0600)
			}
		}
		// A missing/unreadable page must not prevent authenticated config recovery.
		asset := filepath.Join(d.webDir, "app.js")
		if e = os.Rename(asset, asset+".missing"); e != nil {
			t.Fatal(e)
		}
		before := d.api("GET", "config", nil, 200)["runtime"].(map[string]any)["generation"].(float64)
		d.save(200)
		d.auth()
		after := d.api("GET", "config", nil, 200)["runtime"].(map[string]any)["generation"].(float64)
		if after != before+1 {
			t.Fatal("proxy not restarted")
		}
		os.Rename(asset+".missing", asset)
		exchange()
		if permissionChecks {
			// Read-only config files are replaceable when their parent is writable.
			if e = os.Chmod(d.appPath, 0444); e != nil {
				t.Fatal(e)
			}
			d.save(200)
			exchange()
			parent := filepath.Dir(d.appPath)
			beforeFile, _ := os.ReadFile(d.appPath)
			generation := d.api("GET", "config", nil, 200)["runtime"].(map[string]any)["generation"]
			if e = os.Chmod(parent, 0500); e != nil {
				t.Fatal(e)
			}
			d.save(500)
			os.Chmod(parent, 0700)
			afterFile, _ := os.ReadFile(d.appPath)
			if !bytes.Equal(beforeFile, afterFile) ||
				generation != d.api("GET", "config", nil, 200)["runtime"].(map[string]any)["generation"] {
				t.Fatal("failed save changed disk or runtime")
			}
			d.auth()
			d.save(200)
			exchange()
		}
		d.stop()
		d.assertPortsReleased()
		// Failed startup is retried from another cwd against the same account/store.
		page := filepath.Join(d.webDir, "index.html")
		if e = os.Rename(page, page+".missing"); e != nil {
			t.Fatal(e)
		}
		d.failStart(cwds[2], "web-dir")
		os.Rename(page+".missing", page)
		if permissionChecks {
			os.Chmod(page, 0)
			d.failStart(cwds[2], "permission denied")
			os.Chmod(page, 0600)
			os.Chmod(d.webDir, 0)
			d.failStart(cwds[2], "permission denied")
			os.Chmod(d.webDir, 0700)
		}
		d.start(cwds[2], false)
		d.auth()
		d.save(200)
		exchange()
		if _, e = os.Stat(d.usersPath); e != nil {
			t.Fatal("credentials not relative to admin config", e)
		}
		entries, e := os.ReadDir(d.sessionDir)
		if e != nil || len(entries) != 1 {
			t.Fatalf("unexpected persisted sessions %d %v", len(entries), e)
		}
		t.Logf(
			("%s: cwd changed; %s web-dir; live HTML/CSS/JS; missing/permissions restored; " +
				"login/save/restart recovered"),
			d.role,
			mode,
		)
	}
	exchange()
	for _, d := range nodes {
		d.stop()
		d.assertPortsReleased()
	}
	t.Logf("HTTP secure=%t permission checks=%t; both roles and ports released", secure, permissionChecks)
}
func writeDeploymentAtomic(path string, b []byte) error {
	f, e := os.CreateTemp(filepath.Dir(path), ".deploy-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if _, e = f.Write(b); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	return os.Rename(f.Name(), path)
}
