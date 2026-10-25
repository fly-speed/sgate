#!/usr/bin/env python3
"""Build disposable diagnostic binaries through Go overlays, without source edits."""
import argparse, json, pathlib, subprocess, os, hashlib, datetime, tempfile

p = argparse.ArgumentParser()
p.add_argument("--output", default="artifacts/config-noise-soak")
p.add_argument("--race", action="store_true")
p.add_argument("--managed", action="store_true", help="also instrument acl_master entry points")
p.add_argument(
    "--check-registry",
    action="store_true",
    help="also test diagnostic registry cleanup and garbage collection",
)
p.add_argument(
    "--watch-goroutines",
    action="store_true",
    help="capture idle spikes every 20ms with connection/cache state",
)
a = p.parse_args()
repo = pathlib.Path(__file__).resolve().parent.parent
out = (repo / a.output).resolve()
out.mkdir(parents=True, exist_ok=True)
replace = {}


def overlay(rel, source):
    target = out / (rel.replace("/", "_") + ".overlay")
    target.write_text(source)
    replace[str(repo / rel)] = str(target)


def edit_once(source, old, new):
    if source.count(old) != 1:
        raise RuntimeError("diagnostic injection anchor changed: " + old)
    return source.replace(old, new)


# Private census functions exist only in the overlay diagnostic builds.
s = edit_once(
    (repo / "internal/tunnel/client.go").read_text(),
    "return c.run(ctx, listener, &clientSessions{})",
    "state := &clientSessions{}\n configSoakClients.Store(c, state)\n defer configSoakClients.Delete(c)\n return c.run(ctx, listener, state)",
)
s = edit_once(s, '"sync"', '"sync"\n "sync/atomic"')
s = edit_once(s, "timer := time.NewTimer(delay)", "timer := time.NewTimer(delay)\n configSoakReconnectTimers.Add(1)")
s = edit_once(s, "if retryNow {", "configSoakReconnectTimers.Add(-1)\n if retryNow {")
s += r"""
var configSoakNoiseTimers atomic.Int64
var configSoakHeartbeatTimers atomic.Int64
var configSoakReconnectTimers atomic.Int64
var configSoakClients sync.Map
var configSoakServers sync.Map
var configSoakNextServerID atomic.Uint64

// Report only target-selection fields, never credentials or arbitrary queries.
func ConfigSoakEndpoints() []map[string]any {
    endpoints := []map[string]any{}
    configSoakClients.Range(func(key, value any) bool {
        cfg := key.(*Client).cfg
        u, _ := url.Parse(cfg.URL)
        endpoints = append(endpoints, map[string]any{
            "addr": u.Query()["addr"], "path": u.Path,
            "keep": u.Query().Get("keep"), "omit_addr": cfg.OmitAddr,
            "configured_addr": cfg.Addr,
            "offline_timeout_ns": int64(cfg.OfflineTimeout),
        })
        return true
    })
    return endpoints
}

func ConfigSoakCensus() map[string]int64 {
	v := map[string]int64{
		"sessions":       0,
		"client_tunnels": 0,
		"registered_servers": 0,
		"streams":        0,
		"pending_bytes":  0,
		"nonces":         0,
		"attached":       0,
		"noise_sent":     0,
		"noise_received": 0,
        "noise_timers": configSoakNoiseTimers.Load(),
        "heartbeat_timers": configSoakHeartbeatTimers.Load(),
        "reconnect_timers": configSoakReconnectTimers.Load(),
	}
	add := func(s *session) {
		s.mu.Lock()
		defer s.mu.Unlock()
		v["streams"] += int64(len(s.streams))
		v["pending_bytes"] += int64(s.pendingBytes)
		v["noise_sent"] += int64(s.noiseSent)
		v["noise_received"] += int64(s.noiseReceived)
		if s.target != "" {
			v["target_streams:"+s.target] += int64(len(s.streams))
		}
	}
	configSoakClients.Range(func(k, x any) bool {
		state := x.(*clientSessions)
		state.mu.Lock()
		defer state.mu.Unlock()
		if state.current != nil {
			v["client_tunnels"]++
			v["sessions"]++
			add(state.current)
		}
		return true
	})
	configSoakServers.Range(func(k, x any) bool {
		s := x.(*Server)
		s.mu.Lock()
		defer s.mu.Unlock()
		// Closed, empty servers must not be retained by the diagnostic registry.
		// Keep incomplete cleanup registered so nonempty state remains visible.
		if s.closed && len(s.sessions) == 0 && len(s.nonces) == 0 {
			configSoakServers.Delete(k)
			return true
		}
		v["registered_servers"]++
		v["session_ttl_ns"] = int64(s.cfg.SessionTTL)
		v["sessions"] += int64(len(s.sessions))
		v["nonces"] += int64(len(s.nonces))
		for _, e := range s.sessions {
			if e.inUse {
				v["attached"]++
			}
			add(e.session)
		}
		return true
	})
	return v
}
"""
overlay("internal/tunnel/client.go", s)

# 直接记录代理拥有的定时器对象；计数减少前原 Stop defer 已执行。
# 这些观察变量只存在于诊断 overlay，不进入生产程序。
s = (repo / "internal/tunnel/transport.go").read_text()
s = edit_once(s, "defer ticker.Stop()", "configSoakHeartbeatTimers.Add(1)\n defer configSoakHeartbeatTimers.Add(-1)\n defer ticker.Stop()")
s = edit_once(s, "defer noiseTimer.Stop()", "configSoakNoiseTimers.Add(1)\n defer configSoakNoiseTimers.Add(-1)\n defer noiseTimer.Stop()")
overlay("internal/tunnel/transport.go", s)
s = edit_once(
    (repo / "internal/tunnel/server.go").read_text(),
    "go s.reap()",
    "configSoakServers.Store(configSoakNextServerID.Add(1),s)\n go s.reap()",
)
overlay("internal/tunnel/server.go", s)
if a.watch_goroutines:
    s = (repo / "internal/tunnel/stats.go").read_text()
    s = edit_once(
        s,
        "return &trafficConn{Conn: c, stats: s}",
        "tracked := &trafficConn{Conn: c, stats: s}\n configSoakConnections.Store(tracked, true)\n return tracked",
    )
    s = edit_once(
        s,
        "c.once.Do(func() { c.stats.active.Add(-1) })",
        "c.once.Do(func() { c.stats.active.Add(-1); configSoakConnections.Delete(c) })",
    )
    s += r"""
var configSoakConnections sync.Map

func ConfigSoakConnectionCount() int {
    count := 0
    configSoakConnections.Range(func(key, value any) bool { count++; return true })
    return count
}

// Details are captured on trigger only; the fast watcher polls just the census.
func ConfigSoakDetails() map[string]any {
    connections := []map[string]string{}
    configSoakConnections.Range(func(key, value any) bool {
        c := key.(*trafficConn)
        connections = append(connections, map[string]string{"local": c.LocalAddr().String(), "remote": c.RemoteAddr().String()})
        return true
    })
    sessions := []map[string]any{}
    add := func(s *session) {
        s.mu.Lock()
        defer s.mu.Unlock()
        streams := []map[string]any{}
        for id, st := range s.streams {
            streams = append(streams, map[string]any{"id": id, "local": st.conn.LocalAddr().String(), "remote": st.conn.RemoteAddr().String()})
        }
        sessions = append(sessions, map[string]any{"target": s.target, "closed": s.closed, "attached": s.active != nil, "pending_bytes": s.pendingBytes, "pending_frames": len(s.pending), "reset_queue": len(s.resets), "streams": streams})
    }
    configSoakClients.Range(func(key, value any) bool {
        state := value.(*clientSessions)
        state.mu.Lock()
        defer state.mu.Unlock()
        if state.current != nil { add(state.current) }
        return true
    })
    configSoakServers.Range(func(key, value any) bool {
        server := value.(*Server)
        server.mu.Lock()
        defer server.mu.Unlock()
        for _, entry := range server.sessions { add(entry.session) }
        return true
    })
    return map[string]any{"connections": connections, "sessions": sessions}
}
"""
    overlay("internal/tunnel/stats.go", s)
metrics = r"""
func configSoakMetrics() func() {
	config := flag.Lookup("config").Value.String()
	path := config + ".metrics"
	done, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		sample := func(phase string) {
			if phase != "load" {
				debug.FreeOSMemory()
			}
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			stack := make([]byte, 4<<20)
			n := runtime.Stack(stack, true)
			deadline := time.Now().Add(5 * time.Second)
			for phase == "closed" &&
				(bytes.Count(stack[:n], []byte(".(*session).writeTransport(")) != 0 || runtime.NumGoroutine() > 6) &&
				time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
				n = runtime.Stack(stack, true)
			}
			if phase == "closed" {
				if e := os.WriteFile(path+".closed-"+strconv.Itoa(os.Getpid())+".stack", stack[:n], 0600); e != nil {
					panic(e)
				}
			}
			fd := -1
			for _, dir := range []string{"/proc/self/fd", "/dev/fd"} {
				if entries, e := os.ReadDir(dir); e == nil {
					fd = len(entries)
					break
				}
			}
			if fd < 0 && phase != "load" {
				if out, e := exec.Command("lsof", "-a", "-p", strconv.Itoa(os.Getpid()), "-Ff").Output(); e == nil {
					fd = 0
					for _, line := range strings.Split(string(out), "\n") {
						if strings.HasPrefix(line, "f") {
							if _, e := strconv.Atoi(strings.TrimPrefix(line, "f")); e == nil {
								fd++
							}
						}
					}
				}
			}
			rss := int64(-1)
			if b, e := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output(); e == nil {
				if v, e := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); e == nil {
					rss = v * 1024
				}
			}
			// Capture goroutine ownership after the OS probes as well: requests
			// can start or finish while lsof/ps run, especially under admin load.
			n = runtime.Stack(stack, true)
			goroutines := runtime.NumGoroutine()
			if phase != "load" {
				name := path+"."+phase+"-"+strconv.Itoa(os.Getpid())+".stack"
				if e := os.WriteFile(name, stack[:n], 0600); e != nil {
					panic(e)
				}
			}
			v := map[string]any{
				"phase":             phase,
				"unix_nano":         time.Now().UnixNano(),
				"pid":               os.Getpid(),
				"heap_bytes":        m.HeapAlloc,
				"heap_inuse":        m.HeapInuse,
				"rss_bytes":         rss,
				"goroutines":        goroutines,
				"fds":               fd,
				"transport_writers": bytes.Count(stack[:n], []byte(".(*session).writeTransport(")),
				"stack_truncated":   n == len(stack),
				"census":            tunnel.ConfigSoakCensus(),
				"endpoints":         tunnel.ConfigSoakEndpoints(),
				"noise_min":         flag.Lookup("noise-interval-min").Value.String(),
				"noise_max":         flag.Lookup("noise-interval-max").Value.String(),
				"noise_min_bytes":   flag.Lookup("noise-min-bytes").Value.String(),
				"noise_max_bytes":   flag.Lookup("noise-max-bytes").Value.String(),
			}
			b, e := json.Marshal(v)
			if e != nil {
				panic(e)
			}
			f, e := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			if e != nil {
				panic(e)
			}
			_, e = f.Write(append(b, 10))
			f.Close()
			if e != nil {
				panic(e)
			}
		}
		sample("startup")
		for {
			select {
			case <-done:
				sample("closed")
				return
			case <-ticker.C:
				phase := "load"
				if b, e := os.ReadFile(config + ".collect"); e == nil {
					phase = string(b)
					os.Remove(config + ".collect")
				}
				sample(phase)
			}
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}
"""
if a.watch_goroutines:
    metrics = edit_once(
        metrics,
        'path := config + ".metrics"',
        'path := config + ".metrics"\n stopWatch := configSoakWatch(config)',
    )
    metrics = edit_once(metrics, "close(done)", "stopWatch()\n close(done)")
    metrics += (repo / "scripts/soak-goroutine-watch.go.in").read_text()
for role in ["client", "server"]:
    s = (repo / f"cmd/sgate-{role}/main.go").read_text()
    s = edit_once(
        s,
        "import (",
        'import (\n "bytes"\n "encoding/json"\n "runtime"\n "runtime/debug"\n "os/exec"\n "strconv"\n "strings"',
    )
    if role == "client":
        s = edit_once(s, "import (", 'import (\n "time"\n "sgate/internal/tunnel"')
    if role == "server":
        s = edit_once(s, "import (", 'import (\n "time"\n "sgate/internal/tunnel"')
    s = edit_once(
        s,
        "ctx, stop := signal.NotifyContext",
        "stopMetrics:=configSoakMetrics()\n defer stopMetrics()\n ctx, stop := signal.NotifyContext",
    )
    overlay(f"cmd/sgate-{role}/main.go", s + "\n" + metrics)
if a.check_registry:
    source = (repo / "internal/tunnel/session_cleanup_test.go").read_text()
    source = edit_once(source, '"context"', '"context"\n "runtime"')
    source += (repo / "scripts/soak-registry-test.go.in").read_text()
    overlay("internal/tunnel/session_cleanup_test.go", source)
mapfile = out / "overlay.json"
mapfile.write_text(json.dumps({"Replace": replace}, indent=2))
env = os.environ.copy()
env.setdefault("GOMAXPROCS", "4")
env.setdefault("GOPROXY", "off")
env.setdefault(
    "GOCACHE",
    str(
        pathlib.Path(
            "/private/tmp" if pathlib.Path("/private/tmp").is_dir() else tempfile.gettempdir()
        )
        / "sgate-go-cache"
    ),
)
for role in ["client", "server"]:
    cmd = ["go", "build", "-overlay", str(mapfile)]
    if a.race:
        cmd += ["-race"]
    cmd += ["-o", str(out / ("sgate-" + role + ("-race" if a.race else ""))), "./cmd/sgate-" + role]
    subprocess.run(cmd, cwd=repo, env=env, check=True)
if a.check_registry:
    command = ["go", "test", "-overlay", str(mapfile)]
    if a.race:
        command.append("-race")
    command += ["./internal/tunnel", "-run", "^TestDiagnosticRegistry", "-count=10"]
    with (out / "registry-check.log").open("w") as log:
        subprocess.run(command, cwd=repo, env=env, stdout=log, stderr=subprocess.STDOUT, check=True)
print("built diagnostic binaries:", out, "race=", a.race)

manifest = {
    "created_utc": datetime.datetime.now(datetime.timezone.utc).isoformat(),
    "race": a.race,
    "instrumented": True,
    "watch_goroutines": a.watch_goroutines,
    "registry_check": a.check_registry,
    "overlay": str(mapfile),
    "sources": {},
    "build_inputs": {},
    "binaries": {},
}
inputs = [repo / "go.mod", repo / "go.sum", repo / "vendor/modules.txt"]
for pattern in [
    "cmd/**/*.go",
    "internal/**/*.go",
    "vendor/github.com/acl-dev/gin-service/*.go",
    "vendor/github.com/acl-dev/go-service/*.go",
]:
    inputs.extend(path for path in repo.glob(pattern) if not path.name.endswith("_test.go"))
if a.watch_goroutines:
    inputs.append(repo / "scripts/soak-goroutine-watch.go.in")
for path in sorted(inputs):
    manifest["build_inputs"][str(path.relative_to(repo))] = hashlib.sha256(
        path.read_bytes()
    ).hexdigest()
for original, generated in replace.items():
    manifest["sources"][str(pathlib.Path(original).relative_to(repo))] = {
        "original_sha256": hashlib.sha256(pathlib.Path(original).read_bytes()).hexdigest(),
        "overlay_sha256": hashlib.sha256(pathlib.Path(generated).read_bytes()).hexdigest(),
    }
for role in ["client", "server"]:
    f = out / ("sgate-" + role + ("-race" if a.race else ""))
    manifest["binaries"][f.name] = hashlib.sha256(f.read_bytes()).hexdigest()
(out / ("manifest-race.json" if a.race else "manifest-plain.json")).write_text(
    json.dumps(manifest, indent=2) + "\n"
)
