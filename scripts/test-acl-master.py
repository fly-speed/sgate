#!/usr/bin/env python3
"""Isolated acl_master integration; never loads or alters installed service .cf files."""
import argparse, concurrent.futures, contextlib, grp, http.cookiejar, json, threading
import os, pathlib, pwd, signal, socket, ssl, subprocess, tempfile, time, urllib.request

ROOT = pathlib.Path(__file__).resolve().parents[1]


def write(path, text):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text)
    path.chmod(0o600)


def port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def eventually(fn, timeout=15):
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        try:
            value = fn()
            if value:
                return value
        except (OSError, ValueError, urllib.error.URLError, AssertionError) as e:
            last = e
        time.sleep(0.05)
    raise AssertionError("condition timed out: " + str(last))


class Admin:
    def __init__(self, base, context):
        self.context = context
        self.base = base
        self.jar = http.cookiejar.CookieJar()
        self.opener = urllib.request.build_opener(
            urllib.request.HTTPCookieProcessor(self.jar),
            urllib.request.HTTPSHandler(context=context),
        )
        self.csrf = ""

    def call(self, path, data=None, method=None, status=200):
        headers = {"Content-Type": "application/json", "X-CSRF-Token": self.csrf}
        req = urllib.request.Request(
            self.base + "/api/" + path,
            None if data is None else json.dumps(data).encode(),
            headers,
            method=method,
        )
        try:
            response = self.opener.open(req, timeout=10)
        except urllib.error.HTTPError as e:
            response = e
        with response:
            raw = response.read()
            try:
                body = json.loads(raw)
            except ValueError as error:
                # Framework shutdown responses may be plain text. Preserve
                # their status so probes can distinguish a planned 503 from
                # malformed successful responses or authentication failures.
                raise AssertionError(
                    (path, response.code, raw.decode(errors="replace")[:256])
                ) from error
            assert response.code == status, (path, response.code, body)
            return body

    def authenticated(self):
        body = self.call("status")
        assert body["authenticated"]
        self.csrf = body["csrf"]
        return True

    def runtime(self):
        return self.call("config")["runtime"]

    def apply(self, edit=lambda text: text + "\n# apply via Web management\n", status=200):
        cfg = self.call("config")
        return self.call(
            "config",
            {"content": edit(cfg["content"]), "revision": cfg["revision"], "apply": True},
            "PUT",
            status,
        )


class FaultRelay:
    """Raw TCP relay: faults interrupt only the WS/WSS transport, never application sockets."""

    def __init__(self, target):
        self.target = target
        self.listener = socket.socket()
        self.listener.bind(("127.0.0.1", 0))
        self.listener.listen()
        self.listener.settimeout(0.1)
        self.port = self.listener.getsockname()[1]
        self.lock = threading.Lock()
        self.peers = set()
        self.threads = []
        self.done = threading.Event()
        self.acceptor = threading.Thread(target=self.accept)
        self.acceptor.start()
        self.drops = 0

    def accept(self):
        while not self.done.is_set():
            try:
                c, _ = self.listener.accept()
                try:
                    remote = socket.create_connection(self.target, 2)
                except OSError:
                    c.close()
                    continue
                with self.lock:
                    self.peers.update([c, remote])
                for source, dest in [(c, remote), (remote, c)]:
                    t = threading.Thread(target=self.copy, args=(source, dest))
                    self.threads.append(t)
                    t.start()
            except socket.timeout:
                pass
            except OSError:
                break

    def copy(self, source, dest):
        try:
            while not self.done.is_set():
                b = source.recv(8192)
                if not b:
                    break
                dest.sendall(b)
                time.sleep(0.001)
        except OSError:
            pass
        finally:
            for c in [source, dest]:
                with contextlib.suppress(OSError):
                    c.shutdown(socket.SHUT_RDWR)
                c.close()
                with self.lock:
                    self.peers.discard(c)

    def drop(self):
        with self.lock:
            peers = list(self.peers)
        for c in peers:
            with contextlib.suppress(OSError):
                c.shutdown(socket.SHUT_RDWR)
        if peers:
            self.drops += 1

    def close(self):
        self.done.set()
        self.listener.close()
        self.acceptor.join(4)
        self.drop()
        for t in self.threads:
            t.join(4)
        assert not self.acceptor.is_alive() and not any(t.is_alive() for t in self.threads)
        with self.lock:
            assert not self.peers


def stress(
    args,
    case,
    output,
    admin,
    addresses,
    relay,
    ctl,
    pid,
    pid_gone,
    peers,
    peers_lock,
    configs,
    restart_checks=None,
):
    """128 application streams share each tunnel; process restart intentionally closes them."""
    samples = []

    def collect(phase):
        # A restarted client creates a new session. Its old server session must
        # expire under session-ttl; it is deliberately retained until then.
        def drained():
            for role in admin:
                path = pathlib.Path(str(configs[role]) + ".metrics")
                if not path.exists():
                    return False
                lines = path.read_text().splitlines()
                try:
                    v = json.loads(lines[-1])
                except (ValueError, IndexError):
                    return False
                if v["pid"] != pid(role):
                    return False
                if v["census"]["streams"] or v["census"]["pending_bytes"]:
                    return False
                if role == "client" and args.require_client_framework:
                    if admin[role].runtime()["tcp_connections"] != 0:
                        return False
            return no_backend()

        eventually(drained, 35)
        for role in admin:
            write(pathlib.Path(str(configs[role]) + ".collect"), phase)
        found = {}

        def poll():
            for role in admin:
                path = pathlib.Path(str(configs[role]) + ".metrics")
                if not path.exists():
                    continue
                for line in path.read_text().splitlines():
                    try:
                        v = json.loads(line)
                    except ValueError:
                        continue
                    if v["phase"] == phase:
                        found[role] = v
            return len(found) == 2

        eventually(poll, 15)
        for role, v in found.items():
            assert not v["stack_truncated"]
            assert v["census"]["streams"] == 0 and v["census"]["pending_bytes"] == 0, (
                phase,
                role,
                v,
            )
            assert v["transport_writers"] <= 1, (phase, role, v)
            assert v["census"]["noise_sent"] > 0 and v["census"]["noise_received"] > 0, (
                phase,
                role,
                v,
            )
            v["role"] = role
            if role == "client" and args.require_client_framework:
                v["tcp_framework"] = "go-service"
                v["tcp_connections"] = admin[role].runtime()["tcp_connections"]
                assert v["tcp_connections"] == 0, (phase, v)
            samples.append(v)
        eventually(lambda: no_backend())
        return found

    def no_backend():
        with peers_lock:
            return not peers

    def wave(number):
        ready = threading.Barrier(args.concurrency + 1, timeout=30)

        def stream(index):
            payload = (f"wave={number:04d};stream={index:04d};".encode() + bytes(range(256))) * 480
            with socket.create_connection(("127.0.0.1", addresses["client"][index % 2]), 10) as c:
                c.settimeout(30)
                ready.wait()

                def send():
                    c.sendall(payload)
                    c.shutdown(socket.SHUT_WR)

                with concurrent.futures.ThreadPoolExecutor(max_workers=1) as sender:
                    future = sender.submit(send)
                    got = bytearray()
                    while True:
                        b = c.recv(65536)
                        if not b:
                            break
                        got.extend(b)
                        assert len(got) <= len(payload), ("duplicate data", number, index)
                    future.result()
                assert got == payload, ("integrity", number, index, len(got), len(payload))
            return len(payload)

        with concurrent.futures.ThreadPoolExecutor(max_workers=args.concurrency) as pool:
            futures = [pool.submit(stream, index) for index in range(args.concurrency)]
            ready.wait()
            # Each fault waits for the preceding transport to reconnect, then
            # cuts that transport while application readers/writers remain open.
            for _ in range(3):
                eventually(lambda: admin["client"].call("stats")["connections"] == 1, 15)
                time.sleep(0.18)
                relay.drop()
            return sum(f.result() for f in futures)

    time.sleep(0.2)
    baseline = collect("baseline")
    pinned_pids = {role: pid(role) for role in admin}
    total = 0
    management_results, login_sessions = [], []
    for number in range(args.rounds):
        if args.management_stress:
            from shared_admin_load import run as management_load

            management_results.append(
                management_load(
                    args,
                    output,
                    admin,
                    addresses,
                    relay,
                    ctl,
                    pid,
                    pid_gone,
                    number,
                    login_sessions,
                    Admin,
                    eventually,
                    (
                        (lambda: restart_checks.failures(admin, pid, configs))
                        if restart_checks is not None and number == 0
                        else None
                    ),
                )
            )
            collect(f"management-idle-{number}")
        total += wave(number)
        collect(f"idle-{number}")
        if args.no_process_restarts:
            # Both processes stay alive; configuration applies replace proxy
            # workers only. Full-payload waves run separately from applies.
            total += wave(1000 + number)
            values = collect(f"same-pid-idle-{number}")
            for role, value in values.items():
                assert pid(role) == pinned_pids[role] == value["pid"]
                original = baseline[role]
                for key, budget in [
                    ("goroutines", 8),
                    ("fds", 4),
                    ("heap_bytes", 12 * 1024 * 1024),
                ]:
                    if value[key] > original[key] + budget:
                        for config in configs.values():
                            write(
                                pathlib.Path(str(config) + ".snapshot"),
                                f"same-pid {role} {key} growth",
                            )
                        time.sleep(0.3)
                        raise AssertionError((role, key, original, value))
            idle_until = time.monotonic() + args.idle_seconds
            observation = 0
            while time.monotonic() < idle_until:
                collect(f"observed-idle-{number}-{observation}")
                observation += 1
            print(f"{output.name}: same-pid cycle {number+1}/{args.rounds} complete", flush=True)
            continue
        # Keep every application stream alive across a real framework process
        # replacement. Such restarts close streams; only transport faults resume.
        held = []
        try:
            for index in range(args.concurrency):
                c = socket.create_connection(("127.0.0.1", addresses["client"][index % 2]), 10)
                held.append(c)
                c.settimeout(15)
                c.sendall(b"restart-held")
                got = bytearray()
                while len(got) < 12:
                    chunk = c.recv(12 - len(got))
                    assert chunk, "restart probe closed before its payload arrived"
                    got.extend(chunk)
                assert got == b"restart-held"
            role = "client" if number % 2 == 0 else "server"
            old = pid(role)
            ctl(role, "restart")
            eventually(lambda: admin[role].authenticated() and pid(role) != old, 25)
            eventually(lambda: pid_gone(old), 20)
            for c in held:
                try:
                    assert c.recv(1) == b"", "old stream survived process restart"
                except ConnectionResetError:
                    pass
            for a in admin.values():
                a.authenticated()
        finally:
            for c in held:
                c.close()
        # Fresh streams after every replacement have new per-stream markers.
        total += wave(1000 + number)
        collect(f"restart-idle-{number}")
        # Repeated idle observations distinguish cleanup tails from growth;
        # they do not change the final baseline resource bounds below.
        idle_until = time.monotonic() + args.idle_seconds
        observation = 0
        while time.monotonic() < idle_until:
            collect(f"observed-idle-{number}-{observation}")
            observation += 1
        print(f"{output.name}: stress cycle {number+1}/{args.rounds} complete", flush=True)
    final = collect("final-idle")
    for role, v in final.items():
        b = baseline[role]
        assert v["goroutines"] <= b["goroutines"] + 8, (role, "goroutine growth", b, v)
        if b["fds"] >= 0 and v["fds"] >= 0:
            assert v["fds"] <= b["fds"] + 4, (role, "fd growth", b, v)
        assert v["heap_bytes"] <= b["heap_bytes"] + 12 * 1024 * 1024, (role, "heap growth", b, v)
    (output / "stress-samples.json").write_text(json.dumps(samples, indent=2) + "\n")
    return dict(
        rounds=args.rounds,
        concurrency=args.concurrency,
        checked_payload_bytes=total,
        transport_faults=relay.drops,
        process_restarts=0 if args.no_process_restarts else args.rounds,
        framework_restarts=(
            args.rounds if args.mode == "managed" and not args.no_process_restarts else 0
        ),
        persistent_login=True,
        old_streams_closed=True,
        idle_resources_bounded=True,
        client_framework_idle_samples=[
            {key: row[key] for key in ["phase", "pid", "tcp_framework", "tcp_connections"]}
            for row in samples
            if row["role"] == "client" and args.require_client_framework
        ],
        management_load=management_results,
        mixed_seconds=args.mixed_seconds,
        idle_seconds=args.idle_seconds,
        pinned_pids=pinned_pids if args.no_process_restarts else {},
        no_process_restarts=args.no_process_restarts,
    )


def run(args, secure):
    case = pathlib.Path(
        tempfile.mkdtemp(
            prefix="sgate-acl-",
            dir="/private/tmp" if pathlib.Path("/private/tmp").is_dir() else None,
        )
    )
    output = pathlib.Path(args.output).resolve()
    if args.mode == "standalone":
        output = output / "standalone"
    output = output / ("WSS" if secure else "WS")
    output.mkdir(parents=True, exist_ok=True)
    # Preserve logs under the requested output, without storing generated keys/cookies.
    backend = socket.socket()
    backend.bind(("127.0.0.1", 0))
    backend.listen()
    backend.settimeout(0.2)
    finished = threading.Event()
    shutting_down = False
    pinned_processes = {}
    peers = set()
    lock = threading.Lock()
    threads = []

    def echo(c):
        try:
            with c:
                while True:
                    b = c.recv(65536)
                    if not b:
                        break
                    c.sendall(b)
        except OSError:
            pass
        finally:
            with lock:
                peers.discard(c)

    def accept():
        while not finished.is_set():
            try:
                c, _ = backend.accept()
                with lock:
                    peers.add(c)
                thread = threading.Thread(target=echo, args=(c,))
                threads.append(thread)
                thread.start()
            except socket.timeout:
                pass
            except OSError:
                break

    accept_thread = threading.Thread(target=accept)
    accept_thread.start()
    owned_pids = set()
    listener_counts = {}
    master = None
    children = {}
    services = {}
    configs = {}
    admin = {}
    addresses = {role: [port(), port()] for role in ["client", "server"]}
    if args.mode == "standalone":
        addresses = {role: [numbers[0], numbers[0]] for role, numbers in addresses.items()}
    admin_ports = {role: port() for role in ["client", "server"]}
    management = str(case / "var/public/master.sock")
    relay = FaultRelay(("127.0.0.1", addresses["server"][0])) if args.stress else None
    context = ssl.create_default_context()
    restart_checks = None
    if args.restart_failure_checks:
        from web_restart_failures import RestartChecks

        restart_checks = RestartChecks(case, output, args.ctl, management, Admin, eventually)
    for folder in ["conf/service", "var/public", "var/private", "var/pid", "var/log", "work"]:
        (case / folder).mkdir(parents=True, exist_ok=True)
    key = case / "app/sgate.key"
    write(key, os.urandom(32).hex())
    cert, tls_key = case / "app/cert.pem", case / "app/tls.key"
    if secure:
        request = case / "app/openssl.cnf"
        write(
            request,
            "[req]\ndistinguished_name=dn\nx509_extensions=ext\nprompt=no\n[dn]\nCN=localhost\n[ext]\nsubjectAltName=IP:127.0.0.1,DNS:localhost\nbasicConstraints=critical,CA:TRUE\n",
        )
        subprocess.run(
            [
                "openssl",
                "req",
                "-x509",
                "-newkey",
                "rsa:2048",
                "-nodes",
                "-days",
                "1",
                "-keyout",
                str(tls_key),
                "-out",
                str(cert),
                "-config",
                str(request),
            ],
            check=True,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        context.load_verify_locations(str(cert))
    scheme, wire = ("https", "wss") if secure else ("http", "ws")
    for role in ["server", "client"]:
        binary = str(pathlib.Path(getattr(args, role)).resolve())
        cfg = case / f"app/{role}.conf"
        settings = f"listen = 127.0.0.1:1\nkey-file = {key}\npassword = isolated-proxy-password\nnoise-interval-min = 10ms\nnoise-interval-max = 30ms\nnoise-min-bytes = 128\nnoise-max-bytes = 512\n"
        if role == "client":
            settings += f'server = {wire}://127.0.0.1:{relay.port if relay else addresses["server"][0]}/tunnel\n'
            if not args.stress:
                settings += "reconnect-min = 20ms\nreconnect-max = 100ms\n"
            if secure:
                settings += f"tls-ca = {cert}\n"
        else:
            settings += f"target = 127.0.0.1:{backend.getsockname()[1]}\n"
            if not args.stress:
                settings += "session-ttl = 500ms\n"
            if secure:
                settings += f"tls-cert = {cert}\ntls-key = {tls_key}\n"
        if args.stress:
            settings += "max-streams = 256\nheartbeat = 1s\nheartbeat-timeout = 30s\n" + (
                "offline-timeout = 20s\n" if role == "client" else "session-ttl = 20s\n"
            )
        write(cfg, settings)
        acfg = case / f"app/{role}-admin.conf"
        write(
            acfg,
            f"listen = 127.0.0.1:{admin_ports[role]}\nweb-dir = {ROOT}/web/admin\nusers-file = {role}-users.json\nsession-dir = {role}-sessions\nsetup-token-file = {role}-setup.token\nsession-timeout = 1h\n"
            + (f"tls-cert = {cert}\ntls-key = {tls_key}\n" if secure else ""),
        )
        cf = case / f"conf/service/{role}.cf"
        services[role] = cf
        configs[role] = cfg
        bind_host = "" if getattr(args, role + "_wildcard", False) else "127.0.0.1"
        write(
            cf,
            f"""service sgate-test-{role} {{
 master_disable = no
 master_service = {bind_host}|{addresses[role][0]}, {bind_host}|{addresses[role][1]}
 master_type = sock
 master_reuseport = no
 master_status_notify = 0
 master_maxproc = 1
 master_prefork = 1
 master_unpriv = n
 master_chroot = n
 master_private = n
 master_stop_kill = false
 master_stop_wait = true
 master_command = {binary}
 master_args = -config {cfg} -admin-config {acfg}
 master_home = {case}/work
 master_stdout = {output}/{role}.stdout
 master_stderr = {output}/{role}.stderr
 app_use_limit = 0
 app_idle_limit = 0
 app_threads = 0
}}
""",
        )
        if args.merged:
            # A real framework passes -f; no second -config file or admin file.
            text = cf.read_text().replace(
                f"master_args = -config {cfg} -admin-config {acfg}",
                "master_args = -noise-min-bytes 256",
            )
            section = (
                "\n# Integrated proxy and management settings\n"
                + settings
                + "admin_enabled = true\n"
            )
            for line in acfg.read_text().splitlines():
                admin_key, value = line.split("=", 1)
                if admin_key.strip() in ["users-file", "session-dir", "setup-token-file"]:
                    value = str(acfg.parent / value.strip())
                section += (
                    "admin_" + admin_key.strip().replace("-", "_") + " = " + value.strip() + "\n"
                )
            if args.web_service_restart:
                control = (
                    restart_checks.wrapper(role)
                    if restart_checks
                    else str(pathlib.Path(args.ctl).resolve())
                )
                section += f"admin_master_ctl = {control}\n" f"admin_master_socket = {management}\n"
            write(cf, text[: text.rfind("}")] + section + "}\n")
            cfg.unlink()
            acfg.unlink()
            configs[role] = cf
        admin[role] = Admin(
            (
                f"{scheme}://127.0.0.1:{addresses[role][0]}/admin"
                if role == "server"
                else f"{scheme}://127.0.0.1:{admin_ports[role]}"
            ),
            context,
        )
    write(
        case / "conf/main.cf",
        f"""owner_user = {pwd.getpwuid(os.getuid()).pw_name}
owner_group = {grp.getgrgid(os.getgid()).gr_name}
master_owner = {pwd.getpwuid(os.getuid()).pw_name}
default_process_limit = 4
queue_directory = {case}/var
pid_file = {case}/var/pid/master.pid
log_file = {output}/master.log
service_directory = {case}/conf/service
service_file =
daemon_directory = {pathlib.Path(args.master).resolve().parent}
manage_addr = {management}
master_notify_addr = {case}/var/public/monitor.sock
waiting_on_stop = true
stop_kill = true
service_throttle_time = 1
file_exts = .cf
scan_subdir = 0
""",
    )

    def ctl(role, action):
        if args.no_process_restarts and action in ["restart", "stop"] and not shutting_down:
            raise AssertionError(("process replacement forbidden in same-PID soak", role, action))
        if args.web_service_restart and action == "restart":
            if restart_checks:
                restart_checks.restart(role, admin, pid, pid_gone)
                return
            admin[role].call("restart-service", {"expected_pid": pid(role)}, "POST", status=202)
            return
        if args.mode == "standalone":
            if action in ["stop", "restart"]:
                pair = children.pop(role, None)
                if pair:
                    child, log = pair
                    try:
                        if child.poll() is None:
                            child.send_signal(signal.SIGINT)
                        try:
                            child.wait(15)
                        except subprocess.TimeoutExpired:
                            child.kill()
                            child.wait()
                            raise
                        assert child.returncode == 0, (role, child.returncode)
                    finally:
                        log.close()
            if action in ["start", "restart"]:
                assert role not in children
                flags = [
                    "-alone",
                    "-config",
                    str(services[role]),
                    "-listen",
                    f"{'' if role == 'server' and getattr(args, 'server_wildcard', False) else '127.0.0.1'}:{addresses[role][0]}",
                    "-noise-min-bytes",
                    "256",
                ]
                log = (output / (role + ".stderr")).open("ab")
                try:
                    child = subprocess.Popen(
                        [str(pathlib.Path(getattr(args, role)).resolve())] + flags,
                        cwd=case / "work",
                        stdout=log,
                        stderr=log,
                    )
                except BaseException:
                    log.close()
                    raise
                children[role] = (child, log)
                owned_pids.add(child.pid)
            return
        p = subprocess.run(
            [args.ctl, "-s", management, "-f", str(services[role]), "-a", action],
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
            timeout=20,
        )
        with (output / "ctl.log").open("a") as f:
            f.write(role + " " + action + "\n" + p.stdout)
        assert p.returncode == 0, p.stdout

    def exchange(round_no, index):
        payload = (f"round={round_no:03d};stream={index:03d};".encode() + bytes(range(256))) * 256
        with socket.create_connection(("127.0.0.1", addresses["client"][index % 2]), 2) as c:
            c.settimeout(8)
            c.sendall(payload)
            c.shutdown(socket.SHUT_WR)
            got = bytearray()
            while True:
                b = c.recv(65536)
                if not b:
                    break
                got.extend(b)
            assert got == payload, (round_no, index, len(got), len(payload))
        return len(payload)

    def duplex(round_no):
        with concurrent.futures.ThreadPoolExecutor(max_workers=32) as pool:
            return sum(pool.map(lambda index: exchange(round_no, index), range(32)))

    def pid(role):
        rt = admin[role].runtime()
        if args.no_process_restarts:
            assert pinned_processes.setdefault(role, rt["pid"]) == rt["pid"], (
                role,
                pinned_processes,
                rt,
            )
        if role == "server":
            assert rt["http_framework"] == "gin-service" and rt["management_shared"]
        elif args.require_client_framework:
            assert rt["tcp_framework"] == "go-service", rt
        if args.merged:
            assert "noise-min-bytes" in rt["cli_overrides"]
        if args.mode == "managed":
            assert rt["listener_owner"] == "acl_master"
            if getattr(args, role + "_wildcard", False):
                # acl_master may expand each wildcard port into one socket per interface.
                ports = {int(address.rsplit(":", 1)[1]) for address in rt["listeners"]}
                assert ports == set(addresses[role]), rt["listeners"]
            else:
                assert len(rt["listeners"]) == 2
        else:
            assert rt["listener_owner"] == "sgate"
        listener_counts[role] = len(rt["listeners"])
        owned_pids.add(int(rt["pid"]))
        return int(rt["pid"])

    def pid_gone(p):
        try:
            os.kill(p, 0)
            return False
        except ProcessLookupError:
            return True

    result = {
        "mode": args.mode,
        "merged_config": args.merged,
        "secure": secure,
        "listeners_per_role": 2 if args.mode == "managed" else 1,
        "concurrency": 32,
        "payload_bytes": 0,
        "framework_root": str(case),
    }
    try:
        if args.mode == "standalone":
            for role in ["server", "client"]:
                ctl(role, "start")
        else:
            master = subprocess.Popen(
                [args.master, "-c", str(case / "conf"), "-l", str(output / "master.log")],
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
            )
        eventually(lambda: all(admin[role].call("status")["setup_required"] for role in admin))
        for role, a in admin.items():
            token = (case / f"app/{role}-setup.token").read_text().strip()
            a.call(
                "setup",
                {
                    "username": "isolated-admin",
                    "password": "isolated-management-password",
                    "setup_token": token,
                },
            )
            a.authenticated()
            pid(role)
        # The server has no second management listener, including when the
        # legacy admin_listen setting is retained for cookie compatibility.
        with socket.socket() as probe:
            probe.bind(("127.0.0.1", admin_ports["server"]))
            probe.listen()
        admin["server"].call("config")
        with urllib.request.urlopen(
            admin["server"].base + "/", context=context, timeout=5
        ) as response:
            assert b"assets/app.js" in response.read()
        with urllib.request.urlopen(
            admin["server"].base + "/assets/app.js", context=context, timeout=5
        ) as response:
            assert b'fetch("api/"' in response.read()
        for role in ["client", "server"]:
            with urllib.request.urlopen(
                admin[role].base + "/assets/i18n.js", context=context, timeout=5
            ) as response:
                assert response.status == 200
                assert b"const sgateI18n" in response.read()
        Admin(admin["server"].base, context).call("config", status=401)
        origin = admin["server"].base.removesuffix("/admin")
        for path in ["/api/status", "/admin-other/api/status", "/tunnel"]:
            try:
                with urllib.request.urlopen(origin + path, context=context, timeout=5) as response:
                    raise AssertionError(
                        ("unexpected public management or proxy access", path, response.status)
                    )
            except urllib.error.HTTPError as error:
                assert error.code == 404, (path, error.code)
        original_config = configs["server"].read_bytes()
        generation = admin["server"].runtime()["generation"]
        admin["server"].apply(
            lambda text: text.replace("target = ", "path = /admin/api/blocked\ntarget = "), 400
        )
        if secure:
            admin["server"].apply(
                lambda text: text.replace(f"tls-cert = {cert}\n", "").replace(
                    f"tls-key = {tls_key}\n", ""
                ),
                409,
            )
        assert configs["server"].read_bytes() == original_config
        assert admin["server"].runtime()["generation"] == generation
        if args.mode == "managed":
            original = admin["server"].base
            admin["server"].base = f'{scheme}://127.0.0.1:{addresses["server"][1]}/admin'
            try:
                admin["server"].authenticated()
            finally:
                admin["server"].base = original
        if secure:
            # Replace only the proxy certificate, without replacing the HTTP
            # worker executing the apply request. Trust remains the same CA.
            replacement, replacement_key, csr = (
                case / "app/reloaded.pem",
                case / "app/reloaded.key",
                case / "app/reloaded.csr",
            )
            subprocess.run(
                [
                    "openssl",
                    "req",
                    "-new",
                    "-newkey",
                    "rsa:2048",
                    "-nodes",
                    "-keyout",
                    str(replacement_key),
                    "-out",
                    str(csr),
                    "-subj",
                    "/CN=sgate-reloaded",
                ],
                check=True,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
            )
            replacement_key.chmod(0o600)
            subprocess.run(
                [
                    "openssl",
                    "x509",
                    "-req",
                    "-in",
                    str(csr),
                    "-CA",
                    str(cert),
                    "-CAkey",
                    str(tls_key),
                    "-set_serial",
                    "2",
                    "-days",
                    "1",
                    "-extfile",
                    str(request),
                    "-extensions",
                    "ext",
                    "-out",
                    str(replacement),
                ],
                check=True,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
            )
            admin["server"].apply(
                lambda text: text.replace(
                    f"tls-cert = {cert}\n", f"tls-cert = {replacement}\n"
                ).replace(f"tls-key = {tls_key}\n", f"tls-key = {replacement_key}\n")
            )
            with socket.create_connection(("127.0.0.1", addresses["server"][0]), 5) as raw:
                with context.wrap_socket(raw, server_hostname="localhost") as connection:
                    assert connection.getpeercert(binary_form=True) == ssl.PEM_cert_to_DER_cert(
                        replacement.read_text()
                    )
            admin["server"].authenticated()
            # Applying the proxy deliberately closes its old session. Wait for
            # the client to observe that reset and establish a fresh session.
            eventually(lambda: exchange(-1, 99) is not None, 20)
        result["payload_bytes"] += duplex(0)
        # Keep a real application connection active during the first Web apply.
        held = socket.create_connection(("127.0.0.1", addresses["client"][0]), 2)
        held.settimeout(5)
        held.sendall(b"held")
        assert held.recv(4) == b"held"
        for round_no in range(1, 5):
            for role, a in admin.items():
                old = pid(role)
                a.apply()
                a.authenticated()
                assert pid(role) == old
            result["payload_bytes"] += duplex(round_no)
        assert held.recv(1) == b""
        held.close()
        # Framework-owned listener changes are rejected with disk rollback.
        a = admin["client"]
        before = configs["client"].read_bytes()
        old = pid("client")
        if args.mode == "managed":
            a.apply(lambda text: text.replace("listen = 127.0.0.1:1", "listen = 127.0.0.1:2"), 409)
            assert configs["client"].read_bytes() == before
            assert pid("client") == old
        if args.merged:
            # Framework/admin settings cannot be applied by replacing only the
            # proxy worker. Reject and restore the entire service file.
            a.apply(
                lambda text: text.replace(
                    "admin_session_timeout = 1h", "admin_session_timeout = 2h"
                ),
                409,
            )
            assert configs["client"].read_bytes() == before
            assert pid("client") == old
            a.apply(lambda text: text.replace("master_prefork = 1", "master_prefork = 2"), 409)
            assert configs["client"].read_bytes() == before
            assert pid("client") == old
        if args.merged:
            # Gin's registration stays immutable: a saved path change must work
            # through the current proxy, without restarting the HTTP worker.
            original_pids = {role: pid(role) for role in admin}
            admin["server"].apply(
                lambda text: text[: text.rfind("}")]
                + "path = /relocated-tunnel\n"
                + text[text.rfind("}") :]
            )
            admin["client"].apply(lambda text: text.replace("/tunnel\n", "/relocated-tunnel\n"))
            result["payload_bytes"] += duplex(80)
            admin["server"].apply(lambda text: text.replace("path = /relocated-tunnel\n", ""))
            admin["client"].apply(lambda text: text.replace("/relocated-tunnel\n", "/tunnel\n"))
            result["payload_bytes"] += duplex(81)
            for role in admin:
                assert pid(role) == original_pids[role]
                assert admin[role].authenticated()
            result["proxy_path_hot_reload"] = True
        if not args.no_process_restarts:
            for round_no, role in enumerate(["client", "server"], 5):
                if args.merged:
                    cfg = admin[role].call("config")
                    admin[role].call(
                        "config",
                        {
                            "content": cfg["content"].replace(
                                "admin_session_timeout = 1h", "admin_session_timeout = 2h"
                            ),
                            "revision": cfg["revision"],
                            "apply": False,
                        },
                        "PUT",
                    )
                old = pid(role)
                ctl(role, "restart")
                eventually(lambda: admin[role].authenticated() and pid(role) != old)
                eventually(lambda: pid_gone(old))
                eventually(lambda: (exchange(round_no, 99) is not None))
                result["payload_bytes"] += duplex(round_no)
            # Stop uses acl_master's state-channel EOF, not a synthetic SIGTERM.
            for role in ["client", "server"]:
                old = pid(role)
                ctl(role, "stop")
                eventually(lambda: pid_gone(old))
                ctl(role, "start")
                eventually(lambda: admin[role].authenticated() and pid(role) != old)
        eventually(lambda: (exchange(7, 99) is not None))
        result["payload_bytes"] += duplex(7)
        if args.stress:
            result["stress"] = stress(
                args,
                case,
                output,
                admin,
                addresses,
                relay,
                ctl,
                pid,
                pid_gone,
                peers,
                lock,
                configs,
                restart_checks,
            )
        result.update(
            status="passed",
            web_apply_same_pid=True,
            process_restart_new_pid=not args.no_process_restarts,
            persistent_sessions=True,
            metadata_edit_rollback=True,
            server_management_shared=True,
            server_wildcard_listener=getattr(args, "server_wildcard", False),
            client_wildcard_listener=getattr(args, "client_wildcard", False),
            actual_listener_counts=listener_counts,
            legacy_management_port_unused=True,
            shared_routes_auth_and_rollback=True,
            tls_certificate_reload=secure,
            web_service_restart=args.web_service_restart,
            web_restart_checks=restart_checks.results if restart_checks else [],
            client_tcp_framework="go-service" if args.require_client_framework else None,
        )
        if args.mode == "managed":
            result.update(
                framework_restart_new_pid=not args.no_process_restarts,
                listener_edit_rollback=True,
                state_channel_stop=True,
            )
    except BaseException:
        for config in configs.values():
            write(pathlib.Path(str(config) + ".snapshot"), "test failure")
        time.sleep(0.5)
        raise
    finally:
        shutting_down = True
        for role in ["client", "server"]:
            if args.mode == "standalone" or (master is not None and master.poll() is None):
                with contextlib.suppress(Exception):
                    ctl(role, "stop")
        # master_ctl acknowledges the stop request before Go's race runtime
        # finishes its exit delay. Keep the parent alive to reap its children.
        deadline = time.monotonic() + 15
        while master is not None and master.poll() is None and time.monotonic() < deadline:
            if all(pid_gone(p) for p in owned_pids):
                break
            time.sleep(0.05)
        if master is not None and master.poll() is None:
            master.send_signal(signal.SIGTERM)
            try:
                master.wait(12)
            except subprocess.TimeoutExpired:
                master.kill()
                master.wait()
        if relay is not None:
            relay.close()
        finished.set()
        backend.close()
        accept_thread.join(3)
        with lock:
            for peer in list(peers):
                peer.close()
        for thread in threads:
            thread.join(3)
        remaining = [p for p in owned_pids if not pid_gone(p)]
        for p in remaining:
            with contextlib.suppress(ProcessLookupError):
                os.kill(p, signal.SIGTERM)
        assert not remaining, ("child processes retained", remaining)
        for number in sum(addresses.values(), []) + list(admin_ports.values()):
            with socket.socket() as probe:
                probe.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
                probe.bind(("127.0.0.1", number))
        assert not accept_thread.is_alive() and not any(
            thread.is_alive() for thread in threads
        ), "harness thread retained"
        for path in output.glob("*.stderr"):
            assert "WARNING: DATA RACE" not in path.read_text(), path
        result["ports_released"] = True
        result["harness_threads_released"] = True
        for config in configs.values():
            for path in config.parent.glob(config.name + ".metrics*"):
                (output / path.name).write_bytes(path.read_bytes())
        if args.stress:
            closed = {}
            for path in output.glob("*.metrics"):
                for line in path.read_text().splitlines():
                    v = json.loads(line)
                    if v["phase"] != "closed":
                        continue
                    assert v["transport_writers"] == 0, ("transport/noise timer retained", v)
                    assert all(
                        v["census"][k] == 0
                        for k in ["sessions", "streams", "pending_bytes", "nonces", "attached"]
                    ), ("closed census", v)
                    assert v["goroutines"] <= 6, ("closed goroutines", v)
                    closed[v["pid"]] = v
            assert owned_pids <= closed.keys(), (
                "missing shutdown census",
                owned_pids - closed.keys(),
            )
            result["closed_process_censuses"] = len(closed)
            if args.no_process_restarts:
                assert set(closed) == set(pinned_processes.values()), (
                    closed.keys(),
                    pinned_processes,
                )
                result["pinned_processes"] = pinned_processes
        (output / "result.json").write_text(json.dumps(result, indent=2) + "\n")
        import shutil

        shutil.rmtree(case)
    return result


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--client", required=True)
    parser.add_argument("--server", required=True)
    parser.add_argument("--master", default="/opt/soft/acl-master/libexec/acl_master")
    parser.add_argument("--ctl", default="/opt/soft/acl-master/bin/master_ctl")
    parser.add_argument("--output", required=True)
    parser.add_argument(
        "--merged",
        action="store_true",
        help="use one service .cf for proxy, management and framework settings",
    )
    parser.add_argument("--mode", choices=["managed", "standalone"], default="managed")
    parser.add_argument(
        "--server-wildcard",
        action="store_true",
        help="bind the server to all interfaces to verify shared HTTP/HTTPS management",
    )
    parser.add_argument(
        "--client-wildcard",
        action="store_true",
        help="bind client proxy ports to all interfaces; management stays independent",
    )
    parser.add_argument("--tls", choices=["both", "ws", "wss"], default="both")
    parser.add_argument("--stress", action="store_true")
    parser.add_argument(
        "--require-client-framework",
        action="store_true",
        help="verify go-service ownership after every restart and drained framework counters",
    )
    parser.add_argument("--concurrency", type=int, default=128)
    parser.add_argument("--rounds", type=int, default=6)
    parser.add_argument(
        "--no-process-restarts",
        action="store_true",
        help="pin both processes from initial startup to final cleanup",
    )
    parser.add_argument(
        "--continuous-apply-seconds",
        type=float,
        default=0,
        help="apply proxy configuration periodically during sustained load",
    )
    parser.add_argument(
        "--mixed-seconds",
        type=float,
        default=0,
        help="sustain 128 traffic workers and faults before each management cycle",
    )
    parser.add_argument(
        "--idle-seconds",
        type=float,
        default=0,
        help="observe resource cleanup repeatedly after each cycle",
    )
    parser.add_argument(
        "--management-stress",
        action="store_true",
        help="overlap login bursts, save/apply, 128 traffic workers, faults and process restarts",
    )
    parser.add_argument("--login-concurrency", type=int, default=8)
    parser.add_argument("--apply-rounds", type=int, default=6)
    parser.add_argument(
        "--web-service-restart",
        action="store_true",
        help="restart managed services through the authenticated management API",
    )
    parser.add_argument(
        "--restart-failure-checks",
        action="store_true",
        help="inject concurrent clicks, unavailable controls/socket and hung command under load",
    )
    args = parser.parse_args()
    (
        parser.error("standalone stress requires --merged")
        if args.mode == "standalone" and not args.merged
        else None
    )
    (
        parser.error("rounds must be positive and concurrency between 1 and 256")
        if args.rounds < 1 or not 1 <= args.concurrency <= 256
        else None
    )
    if args.management_stress and not (args.stress and args.merged):
        parser.error("--management-stress requires --stress --merged")
    if args.mixed_seconds < 0 or args.idle_seconds < 0:
        parser.error("mixed/idle seconds must be nonnegative")
    if args.continuous_apply_seconds < 0:
        parser.error("continuous apply seconds must be nonnegative")
    if args.no_process_restarts and (args.web_service_restart or args.restart_failure_checks):
        parser.error("same-PID soak cannot enable process restart checks")
    if args.continuous_apply_seconds and not args.management_stress:
        parser.error("continuous apply requires management stress")
    if args.web_service_restart and (not args.merged or args.mode != "managed"):
        parser.error("--web-service-restart requires --merged --mode managed")
    if args.restart_failure_checks and not (args.web_service_restart and args.management_stress):
        parser.error("--restart-failure-checks requires --web-service-restart --management-stress")
    if not 1 <= args.login_concurrency <= 16 or args.apply_rounds < 2:
        parser.error("login concurrency must be 1..16 and apply rounds >= 2")
    results = [
        run(args, secure)
        for secure in [False, True]
        if args.tls == "both" or secure == (args.tls == "wss")
    ]
    print(json.dumps(results))


if __name__ == "__main__":
    main()
