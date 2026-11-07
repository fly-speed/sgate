#!/usr/bin/env python3
"""Isolated deployment matrix, seeded with pre-merge accounts and login cookies.
Requires diagnostic binaries built with build-config-soak.py --managed.
"""
import argparse, concurrent.futures, contextlib, grp, hashlib, json, os, pathlib
import pwd, shutil, signal, socket, ssl, subprocess, sys, tempfile, threading, time
from importlib.util import spec_from_file_location, module_from_spec

sys.dont_write_bytecode = True
spec = spec_from_file_location("acl_test", pathlib.Path(__file__).with_name("test-acl-master.py"))
h = module_from_spec(spec)
spec.loader.exec_module(h)


def gone(pid):
    try:
        os.kill(pid, 0)
        return False
    except ProcessLookupError:
        return True


def run(args, managed, secure, relative):
    name = f'{"managed" if managed else "standalone"}-{"WSS" if secure else "WS"}-{"relative" if relative else "absolute"}'
    output = pathlib.Path(args.output).resolve() / name
    output.mkdir(parents=True, exist_ok=True)
    (output / "result.json").unlink(missing_ok=True)
    case = pathlib.Path(
        tempfile.mkdtemp(
            prefix="sgate-deploy-",
            dir="/private/tmp" if pathlib.Path("/private/tmp").is_dir() else None,
        )
    )
    for folder in [
        "config",
        "assets",
        "runtime/legacy/work",
        "runtime/one/work",
        "runtime/two/work",
        "var/public",
        "var/private",
        "var/pid",
    ]:
        (case / folder).mkdir(parents=True)
    shutil.copytree(h.ROOT / "web/admin", case / "web/admin")
    h.write(case / "assets/key", os.urandom(32).hex())
    ctx = ssl.create_default_context()
    cert = case / "assets/cert"
    tlskey = case / "assets/tls.key"
    if secure:
        h.write(
            case / "assets/openssl.cnf",
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
                str(tlskey),
                "-out",
                str(cert),
                "-config",
                str(case / "assets/openssl.cnf"),
            ],
            check=True,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        ctx.load_verify_locations(str(cert))
    backend = socket.socket()
    backend.bind(("127.0.0.1", 0))
    backend.listen()
    backend.settimeout(0.1)
    done = threading.Event()
    lock = threading.Lock()
    peers = set()
    workers = []

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
        while not done.is_set():
            try:
                c, _ = backend.accept()
                with lock:
                    peers.add(c)
                t = threading.Thread(target=echo, args=(c,))
                workers.append(t)
                t.start()
            except socket.timeout:
                pass
            except OSError:
                break

    acceptor = threading.Thread(target=accept)
    acceptor.start()
    ports = {r: h.port() for r in ["client", "server"]}
    admin_ports = {r: h.port() for r in ports}
    configs = {r: case / f"config/{r}.cf" for r in ports}
    legacy = {r: case / f"config/{r}.conf" for r in ports}
    admin_files = {r: case / f"config/{r}-admin.conf" for r in ports}
    state = {r: case / f"state data/{r}" for r in ports}
    admins = {
        r: h.Admin(
            (
                f'{"https" if secure else "http"}://127.0.0.1:{ports[r]}/admin'
                if r == "server"
                else f'{"https" if secure else "http"}://127.0.0.1:{admin_ports[r]}'
            ),
            ctx,
        )
        for r in ports
    }
    children = {}
    owned = set()
    master = None
    management = str(case / "var/public/master.sock")
    cookie_records = {}
    credentials = {}
    samples = []

    def pvalue(path, base):
        return os.path.relpath(path, base) if relative else str(path)

    def literal(v):
        return json.dumps(v, ensure_ascii=False)

    def app(role):
        values = {
            "key-file": "../deliberately-missing-key",
            "password": "deployment-proxy-secret",
            "noise-interval-min": "10ms",
            "noise-interval-max": "30ms",
            "noise-min-bytes": "128",
            "noise-max-bytes": "512",
        }
        if role == "client":
            values.update(
                server=f'{"wss" if secure else "ws"}://127.0.0.1:{ports["server"]}/tunnel',
                **{"reconnect-min": "20ms", "reconnect-max": "100ms", "offline-timeout": "5s"},
            )
        else:
            values.update(target=f"127.0.0.1:{backend.getsockname()[1]}", **{"session-ttl": "1s"})
        if secure:
            if role == "server":
                values.update(
                    {
                        "tls-cert": pvalue(cert, case / "runtime/one/work"),
                        "tls-key": pvalue(tlskey, case / "runtime/one/work"),
                    }
                )
            else:
                values["tls-ca"] = pvalue(cert, case / "runtime/one/work")
        return values

    def admin_values(role):
        values = {
            "listen": f"127.0.0.1:{admin_ports[role]}",
            "web-dir": pvalue(case / "web/admin", case / "config"),
            "users-file": pvalue(state[role] / "users.json", case / "config"),
            "session-dir": pvalue(state[role] / "sessions", case / "config"),
            "setup-token-file": pvalue(state[role] / "setup.token", case / "config"),
            "session-timeout": "1h",
        }
        if secure:
            values.update(
                {
                    "tls-cert": pvalue(cert, case / "config"),
                    "tls-key": pvalue(tlskey, case / "config"),
                }
            )
        return values

    def cli(role):
        return [
            "-key-file",
            pvalue(case / "assets/key", case / "runtime/one/work"),
            "-noise-min-bytes",
            "256",
        ]

    def write_merged(role):
        extra = {
            "master_disable": "no",
            "master_service": f"127.0.0.1|{ports[role]}",
            "master_type": "sock",
            "master_reuseport": "no",
            "master_status_notify": "0",
            "master_maxproc": "1",
            "master_prefork": "1",
            "master_unpriv": "n",
            "master_chroot": "n",
            "master_private": "n",
            "master_stop_kill": "false",
            "master_stop_wait": "true",
            "master_command": str(pathlib.Path(getattr(args, role)).resolve()),
            "master_args": " ".join(cli(role)),
            "master_home": str(case / "runtime/one/work"),
            "master_stdout": str(output / (role + ".stdout")),
            "master_stderr": str(output / (role + ".stderr")),
            "app_use_limit": "0",
            "app_idle_limit": "0",
            "app_threads": "0",
        }
        text = f"service deployment-{role} {{\n" + "".join(
            f" {k} = {v}\n" for k, v in extra.items()
        )
        text += (
            "".join(f" {k} = {literal(v)}\n" for k, v in app(role).items())
            + " admin_enabled = true\n"
        )
        text += (
            "".join(
                f' admin_{k.replace("-","_")} = {literal(v)}\n'
                for k, v in admin_values(role).items()
            )
            + "}\n"
        )
        h.write(configs[role], text)

    def start_direct(role, old=False, alternate=False):
        cwd = case / (
            "runtime/legacy/work"
            if old
            else "runtime/two/work" if alternate else "runtime/one/work"
        )
        cfg = legacy[role] if old else configs[role]
        flags = ["-alone", "-config" if old or role == "server" else "-f", pvalue(cfg, cwd)]
        if old:
            flags += ["-admin-config", pvalue(admin_files[role], cwd)]
        flags += ["-master_service", f"127.0.0.1:{ports[role]}"] + cli(role)
        log = (output / (role + ".direct.log")).open("ab")
        p = subprocess.Popen(
            [str(pathlib.Path(getattr(args, role)).resolve())] + flags,
            cwd=cwd,
            stdout=log,
            stderr=log,
        )
        children[role] = (p, log)
        owned.add(p.pid)

    def stop_direct(role):
        pair = children.pop(role, None)
        if not pair:
            return
        p, log = pair
        if p.poll() is None:
            p.send_signal(signal.SIGINT)
        try:
            p.wait(12)
        except subprocess.TimeoutExpired:
            p.kill()
            p.wait()
            raise
        finally:
            log.close()
        assert p.returncode == 0, (role, p.returncode)

    def ctl(role, action):
        p = subprocess.run(
            [args.ctl, "-s", management, "-f", str(configs[role]), "-a", action],
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
            timeout=20,
        )
        with (output / "ctl.log").open("a") as f:
            f.write(role + " " + action + "\n" + p.stdout)
        assert p.returncode == 0

    def identify(role):
        rt = admins[role].runtime()
        owned.add(int(rt["pid"]))
        if role == "server":
            assert rt["http_framework"] == "gin-service"
        assert rt["listener_owner"] == ("acl_master" if managed else "sgate"), rt
        assert "noise-min-bytes" in rt["cli_overrides"] and "key-file" in rt["cli_overrides"]
        return int(rt["pid"])

    def identity():
        for r, a in admins.items():
            a.authenticated()
            assert not a.call("status")["setup_required"]
            with a.opener.open(a.base + "/", timeout=5) as page:
                assert page.status == 200 and len(page.read()) > 100
            assert (state[r] / "users.json").read_bytes() == credentials[r]
            for path, data in cookie_records[r].items():
                assert path.read_bytes() == data, "pre-merge session changed"

    def exchange(i):
        payload = (f"case={name};unique={i};".encode() + bytes(range(256))) * 128
        with socket.create_connection(("127.0.0.1", ports["client"]), 3) as c:
            c.settimeout(10)
            c.sendall(payload)
            c.shutdown(socket.SHUT_WR)
            got = bytearray()
            while True:
                b = c.recv(65536)
                if not b:
                    break
                got.extend(b)
                assert len(got) <= len(payload), "extra bytes"
            assert got == payload, ("integrity", i, len(got), len(payload))

    def batch(wave):
        h.eventually(lambda: exchange(-wave - 1) is None)
        with concurrent.futures.ThreadPoolExecutor(max_workers=32) as pool:
            list(pool.map(exchange, range(wave * 32, (wave + 1) * 32)))

    def rows(path):
        if not path.exists():
            return []
        values = []
        for line in path.read_text().splitlines():
            try:
                values.append(json.loads(line))
            except ValueError:
                pass
        return values

    def idle(phase):
        ids = {r: identify(r) for r in ports}

        def drained():
            with lock:
                if peers:
                    return False
            for r in ports:
                values = rows(pathlib.Path(str(configs[r]) + ".metrics"))
                if not values:
                    return False
                v = values[-1]
                if v["pid"] != ids[r] or v["census"]["streams"] or v["census"]["pending_bytes"]:
                    return False
            return True

        h.eventually(drained, 15)
        for r in ports:
            h.write(pathlib.Path(str(configs[r]) + ".collect"), phase)
        for r in ports:

            def get():
                return next(
                    (
                        v
                        for v in reversed(rows(pathlib.Path(str(configs[r]) + ".metrics")))
                        if v["phase"] == phase and v["pid"] == ids[r]
                    ),
                    None,
                )

            v = h.eventually(get, 12)
            v["role"] = r
            samples.append(v)
            inventory = subprocess.run(
                ["lsof", "-a", "-p", str(ids[r]), "-Fftn"],
                stdout=subprocess.PIPE,
                stderr=subprocess.DEVNULL,
                text=True,
                timeout=5,
            )
            assert inventory.returncode == 0, "descriptor inventory unavailable"
            if managed:
                assert (
                    management not in inventory.stdout
                ), "inherited master control socket retained"
            (output / (phase + "-" + r + ".fds")).write_text(inventory.stdout)
            assert (
                v["transport_writers"] == 1
                and v["census"]["sessions"] == 1
                and v["census"]["streams"] == 0
                and v["census"]["pending_bytes"] == 0
            )
            assert v["census"]["noise_sent"] > 0 and v["census"]["noise_received"] > 0
            assert v["noise_min_bytes"] == "256" and not v["stack_truncated"]
            assert (
                admins[r].call("stats")["connections"] >= 1
                if r == "server"
                else admins[r].call("stats")["connections"] == 1
            )

    result = {
        "mode": "managed" if managed else "standalone",
        "secure": secure,
        "relative_paths": relative,
        "status": "running",
        "restart_rounds": args.restarts,
    }
    try:
        # Seed the original account/cookie through the previous split-file API.
        for r in ["server", "client"]:
            h.write(legacy[r], "".join(f"{k} = {literal(v)}\n" for k, v in app(r).items()))
            h.write(
                admin_files[r], "".join(f"{k} = {literal(v)}\n" for k, v in admin_values(r).items())
            )
            start_direct(r, old=True)
            h.eventually(lambda: admins[r].call("status")["setup_required"])
            admins[r].call(
                "setup",
                {
                    "username": "existing-admin",
                    "password": "existing-admin-password",
                    "setup_token": (state[r] / "setup.token").read_text().strip(),
                },
            )
            admins[r].authenticated()
            credentials[r] = (state[r] / "users.json").read_bytes()
            cookie_records[r] = {p: p.read_bytes() for p in (state[r] / "sessions").glob("*.json")}
            assert cookie_records[r]
        batch(0)
        for r in ["client", "server"]:
            stop_direct(r)
        for r in ports:
            write_merged(r)
            legacy[r].unlink()
            admin_files[r].unlink()
        if managed:
            h.write(
                case / "main.cf",
                f"owner_user = {pwd.getpwuid(os.getuid()).pw_name}\nowner_group = {grp.getgrgid(os.getgid()).gr_name}\nmaster_owner = {pwd.getpwuid(os.getuid()).pw_name}\ndefault_process_limit = 4\nqueue_directory = {case}/var\npid_file = {case}/var/pid/master.pid\nlog_file = {output}/master.log\nservice_directory = {case}/config\nservice_file =\ndaemon_directory = {pathlib.Path(args.master).resolve().parent}\nmanage_addr = {management}\nmaster_notify_addr = {case}/var/public/monitor.sock\nwaiting_on_stop = true\nstop_kill = true\nfile_exts = .cf\nscan_subdir = 0\n",
            )
            master = subprocess.Popen(
                [args.master, "-c", str(case), "-l", str(output / "master.log")],
                cwd=case / "runtime/two/work",
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
            )
        else:
            for r in ["server", "client"]:
                start_direct(r)
        h.eventually(lambda: all(a.authenticated() for a in admins.values()), 20)
        identity()
        batch(1)
        idle("baseline")
        for r, a in admins.items():
            before = configs[r].read_bytes()
            a.apply(lambda text: text.replace("admin_enabled = true", "admin_enabled = false"), 409)
            assert configs[r].read_bytes() == before
            old = identify(r)
            a.apply(
                lambda text: text.replace('noise-max-bytes = "512"', 'noise-max-bytes = "1024"')
            )
            assert identify(r) == old
        batch(2)
        identity()
        idle("after-apply")
        # Save-only, then restart from a second working directory/home.
        for r, a in admins.items():
            cfg = a.call("config")
            a.call(
                "config",
                {
                    "content": cfg["content"] + "\n# saved before deployment restart\n",
                    "revision": cfg["revision"],
                    "apply": False,
                },
                "PUT",
            )
        for restart_no in range(args.restarts):
            for r in ["client", "server"]:
                old = identify(r)
                if managed:
                    ctl(r, "stop")
                    h.eventually(lambda: gone(old), 20)
                else:
                    stop_direct(r)
                if managed:
                    h.write(
                        configs[r],
                        configs[r]
                        .read_text()
                        .replace(str(case / "runtime/one/work"), str(case / "runtime/two/work")),
                    )
                    ctl(r, "start")
                else:
                    start_direct(r, alternate=True)
                h.eventually(lambda: admins[r].authenticated() and identify(r) != old, 20)
            identity()
            batch(3 + restart_no)
            idle("after-restart-" + str(restart_no))
        # Verify password login against the original user file as well as the old cookie.
        for r in ports:
            fresh = h.Admin(admins[r].base, ctx)
            fresh.call(
                "login", {"username": "existing-admin", "password": "existing-admin-password"}
            )
            fresh.authenticated()
        identity()
        for r in ports:
            baseline = next(v for v in samples if v["phase"] == "baseline" and v["role"] == r)
            final = next(
                v
                for v in samples
                if v["phase"] == "after-restart-" + str(args.restarts - 1) and v["role"] == r
            )
            assert (
                final["goroutines"] <= baseline["goroutines"] + 6
                and final["fds"] <= baseline["fds"] + 3
            )
            assert final["heap_bytes"] <= baseline["heap_bytes"] + 8 * 1024 * 1024
        result.update(
            status="passed",
            legacy_account_and_cookie=True,
            cli_override=True,
            duplex=True,
            save_apply_restart=True,
            different_cwd=True,
        )
    finally:
        # Discover workers even if an early startup failure prevents API login.
        for metrics in (case / "config").glob("*.metrics"):
            for v in rows(metrics):
                owned.add(v["pid"])
        cleanup = []
        for r in list(children):
            try:
                stop_direct(r)
            except Exception as e:
                cleanup.append(str(e))
        if master is not None and master.poll() is None:
            for r in ports:
                with contextlib.suppress(Exception):
                    ctl(r, "stop")
            with contextlib.suppress(Exception):
                h.eventually(lambda: all(gone(p) for p in owned), 20)
            master.send_signal(signal.SIGTERM)
            try:
                master.wait(15)
            except subprocess.TimeoutExpired:
                master.kill()
                master.wait()
                cleanup.append("master forced stop")
        done.set()
        backend.close()
        acceptor.join(3)
        with lock:
            for c in peers:
                with contextlib.suppress(OSError):
                    c.shutdown(socket.SHUT_RDWR)
                c.close()
        for t in workers:
            t.join(3)
        assert not cleanup, cleanup
        assert not acceptor.is_alive() and not any(t.is_alive() for t in workers)
        assert all(gone(p) for p in owned), "old process retained"
        for number in list(ports.values()) + list(admin_ports.values()):
            with socket.socket() as s:
                s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
                s.bind(("127.0.0.1", number))
        closed = {}
        for path in (case / "config").glob("*.metrics*"):
            shutil.copyfile(path, output / path.name)
            if path.name.endswith(".metrics"):
                for v in rows(path):
                    if v["phase"] == "closed":
                        assert v["goroutines"] <= 6 and v["transport_writers"] == 0, (
                            "closed goroutines",
                            v,
                        )
                        assert all(
                            v["census"][k] == 0
                            for k in ["sessions", "streams", "pending_bytes", "nonces", "attached"]
                        ), ("closed cache", v)
                        closed[v["pid"]] = v
        assert owned == closed.keys(), ("missing closed census", owned - closed.keys())
        for p in list(output.glob("*.log")) + list(output.glob("*.stderr")):
            assert "WARNING: DATA RACE" not in p.read_text()
        result.update(
            ports_released=True,
            processes_released=True,
            cache_goroutine_census=True,
            closed_processes=len(closed),
        )
        (output / "samples.json").write_text(json.dumps(samples, indent=2) + "\n")
        (output / "result.json").write_text(json.dumps(result, indent=2) + "\n")
        shutil.rmtree(case)
    print(name + ": passed", flush=True)
    return result


if __name__ == "__main__":
    p = argparse.ArgumentParser()
    p.add_argument("--client", required=True)
    p.add_argument("--server", required=True)
    p.add_argument("--output", required=True)
    p.add_argument("--master", default="/opt/soft/acl-master/libexec/acl_master")
    p.add_argument("--ctl", default="/opt/soft/acl-master/bin/master_ctl")
    p.add_argument("--mode", choices=["both", "standalone", "managed"], default="both")
    p.add_argument("--tls", choices=["both", "ws", "wss"], default="both")
    p.add_argument("--paths", choices=["both", "relative", "absolute"], default="both")
    p.add_argument("--restarts", type=int, default=1)
    args = p.parse_args()
    assert args.restarts > 0
    results = [
        run(args, m, s, r)
        for m in [False, True]
        for s in [False, True]
        for r in [True, False]
        if (args.mode == "both" or m == (args.mode == "managed"))
        and (args.tls == "both" or s == (args.tls == "wss"))
        and (args.paths == "both" or r == (args.paths == "relative"))
    ]
    print(json.dumps(results))
