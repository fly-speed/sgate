#!/usr/bin/env python3
"""Real standalone/acl_master multi-target tests, using only isolated test services."""

import argparse
import concurrent.futures
import contextlib
import grp
import hashlib
import importlib.util
import json
import os
import pathlib
import pwd
import shutil
import signal
import socket
import ssl
import struct
import subprocess
import tempfile
import threading
import time
import urllib.parse

ROOT = pathlib.Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("acl_fixture", ROOT / "scripts/test-acl-master.py")
common = importlib.util.module_from_spec(spec)
spec.loader.exec_module(common)
write, eventually, port = common.write, common.eventually, common.port
SIZE = 1024
HELLO = struct.Struct("!cIII")


def payload(direction, marker, wave, index, sequence):
    tag = f"{direction}/{marker}/{wave}/{index}/{sequence}/".encode()
    return (tag + hashlib.sha256(tag).digest() * 32)[:SIZE]


def receive(connection, count):
    result = bytearray()
    while len(result) < count:
        chunk = connection.recv(count - len(result))
        if not chunk:
            raise EOFError((len(result), count))
        result.extend(chunk)
    return bytes(result)


class Backend:
    def __init__(self, marker):
        self.marker = marker
        self.listener = socket.socket()
        self.listener.bind(("127.0.0.1", 0))
        self.listener.listen(256)
        self.listener.settimeout(0.1)
        self.address = f"127.0.0.1:{self.listener.getsockname()[1]}"
        self.done = threading.Event()
        self.lock = threading.Lock()
        self.peers, self.threads, self.errors = set(), [], []
        self.records = 0
        self.identities = set()
        self.acceptor = threading.Thread(target=self.accept)
        self.acceptor.start()

    def accept(self):
        while not self.done.is_set():
            try:
                connection, _ = self.listener.accept()
            except socket.timeout:
                continue
            except OSError:
                return
            with self.lock:
                self.peers.add(connection)
            thread = threading.Thread(target=self.stream, args=(connection,))
            self.threads.append(thread)
            thread.start()

    def stream(self, connection):
        sender = None
        count = None
        wave = index = None
        sequence = -1
        stage = "hello"
        stopped = threading.Event()
        errors = []
        try:
            connection.settimeout(35)
            marker, wave, index, count = HELLO.unpack(receive(connection, HELLO.size))
            assert marker.decode() == self.marker, (
                "wrong backend",
                marker,
                self.marker,
                wave,
                index,
            )
            if count and getattr(self, "strict_identities", False):
                with self.lock:
                    identity = (wave, index)
                    assert identity not in self.identities, (
                        "duplicate backend stream",
                        self.marker,
                        identity,
                    )
                    self.identities.add(identity)
            if count == 0:
                connection.sendall(payload("down", self.marker, wave, index, 0))
                assert (
                    connection.recv(1) == b""
                ), "held connection received unexpected upstream data"
                return

            # 在线配置压力测试先完成128路屏障，避免屏障期间预发整段下行，
            # 将尚未开始消费的测试流量积压在系统 socket 缓存中。
            first = 0
            if getattr(self, "defer_downstream", False):
                connection.sendall(payload("down", self.marker, wave, index, 0))
                stage = "first-upstream"
                assert receive(connection, SIZE) == payload("up", self.marker, wave, index, 0)
                with self.lock:
                    self.records += 1
                first = 1

            def send():
                try:
                    for sequence in range(first, count):
                        if stopped.is_set():
                            return
                        connection.sendall(payload("down", self.marker, wave, index, sequence))
                        time.sleep(0.005)
                    connection.sendall(payload("down-end", self.marker, wave, index, count))
                except OSError as error:
                    errors.append(str(error))

            sender = threading.Thread(target=send)
            sender.start()
            stage = "upstream"
            for sequence in range(first, count):
                assert receive(connection, SIZE) == payload(
                    "up", self.marker, wave, index, sequence
                ), ("upstream loss/duplicate/cross-stream", self.marker, wave, index, sequence)
                with self.lock:
                    self.records += 1
            stage = "up-end"
            assert receive(connection, SIZE) == payload("up-end", self.marker, wave, index, count)
            sender.join(35)
            assert not sender.is_alive() and not errors, ("downstream sender", errors)
        except (OSError, EOFError) as error:
            if count != 0 and not self.done.is_set():
                with self.lock:
                    self.errors.append(
                        f"{error!r}; backend={self.marker}, wave={wave}, index={index}, "
                        f"stage={stage}, sequence={sequence}, count={count}"
                    )
        except BaseException as error:
            if not self.done.is_set():
                with self.lock:
                    self.errors.append(
                        f"{error!r}; backend={self.marker}, wave={wave}, index={index}, "
                        f"stage={stage}, sequence={sequence}, count={count}"
                    )
        finally:
            stopped.set()
            with contextlib.suppress(OSError):
                connection.shutdown(socket.SHUT_RDWR)
            connection.close()
            if sender:
                sender.join(3)
            with self.lock:
                self.peers.discard(connection)

    def active(self):
        with self.lock:
            return len(self.peers)

    def close(self):
        self.done.set()
        self.listener.close()
        self.acceptor.join(3)
        with self.lock:
            for connection in self.peers:
                with contextlib.suppress(OSError):
                    connection.shutdown(socket.SHUT_RDWR)
        for thread in self.threads:
            thread.join(3)
        assert not self.acceptor.is_alive() and not any(t.is_alive() for t in self.threads)
        assert self.active() == 0


class Case:
    def __init__(self, args):
        self.args = args
        self.output = pathlib.Path(args.output).resolve()
        assert not self.output.exists(), "case output must be new"
        self.output.mkdir(parents=True)
        self.root = pathlib.Path(tempfile.mkdtemp(prefix="sgate-multibackend-", dir="/private/tmp"))
        for folder in ["conf/service", "var/public", "var/private", "var/pid", "var/log", "work"]:
            (self.root / folder).mkdir(parents=True, exist_ok=True)
        self.backends = {marker: Backend(marker) for marker in "ABCD"}
        self.targets = {key: backend.address for key, backend in self.backends.items()}
        self.targets["C"] = "localhost:" + self.targets["C"].split(":")[1]
        self.roles = ["server", "client0", "client1", "client2"]
        self.ports = {role: port() for role in self.roles}
        self.relay = common.FaultRelay(("127.0.0.1", self.ports["server"]))
        self.context = ssl.create_default_context()
        self.scheme = "https" if args.tls == "wss" else "http"
        self.configs, self.processes, self.pids = {}, {}, {}
        self.master = None
        self.client_flags = {}
        self.samples, self.waves, self.probes = [], [], []
        self.default = "A"
        self.management = str(self.root / "var/public/master.sock")
        self.key = os.urandom(32).hex()
        write(self.root / "key", self.key)
        self.cert = self.root / "cert.pem"
        self.setup()

    def setup(self):
        if self.args.tls == "wss":
            request = self.root / "openssl.cnf"
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
                    str(self.root / "tls.key"),
                    "-out",
                    str(self.cert),
                    "-config",
                    str(request),
                ],
                check=True,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
            )
            self.context.load_verify_locations(str(self.cert))
        for role in self.roles:
            binary_role = "server" if role == "server" else "client"
            binary = pathlib.Path(getattr(self.args, binary_role)).resolve()
            config = self.root / f"conf/service/{role}.cf"
            self.configs[role] = config
            application = f"listen = 127.0.0.1:{self.ports[role]}\nkey-file = {self.root}/key\npassword = isolated-target-password\nmax-streams = 128\nnoise-interval-min = 10ms\nnoise-interval-max = 30ms\nnoise-min-bytes = 128\nnoise-max-bytes = 512\nheartbeat = 1s\nheartbeat-timeout = 30s\n"
            if role == "server":
                application += f"target = {self.targets['A']}\nallowed-targets = {self.targets['B']},{self.targets['C']}\nsession-ttl = 6s\nadmin_enabled = true\nadmin_web_dir = {ROOT}/web/admin\nadmin_listen = 127.0.0.1:9082\nadmin_users_file = {self.root}/users.json\nadmin_session_dir = {self.root}/sessions\nadmin_setup_token_file = {self.root}/setup.token\nadmin_session_timeout = 1h\n"
                if self.args.tls == "wss":
                    application += f"tls-cert = {self.cert}\ntls-key = {self.root}/tls.key\n"
            else:
                index = int(role[-1])
                query = (
                    ""
                    if index == 0
                    else "?addr="
                    + urllib.parse.quote(self.targets["B" if index == 1 else "C"], safe="")
                )
                if self.args.selection == "addr" and index != 0:
                    query = "?addr=" + urllib.parse.quote(self.targets["A"], safe="") + "&keep=1"
                application += f'server = "{self.args.tls}://127.0.0.1:{self.relay.port}/tunnel{query}"\noffline-timeout = 6s\n'
                if self.args.selection == "addr" and index != 0:
                    # client1 使用文件 addr；client2 用命令行覆盖文件的另一个目标。
                    selected = self.targets["B"] if index == 1 else self.targets["A"]
                    application += f"addr = {selected}\n"
                    if index == 2:
                        self.client_flags[role] = ["-addr", self.targets["C"]]
                if self.args.tls == "wss":
                    application += f"tls-ca = {self.cert}\n"
            write(
                config,
                f"""service multi-{role} {{
master_disable = no
master_service = 127.0.0.1|{self.ports[role]}
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
master_args = -noise-min-bytes 256 {" ".join(self.client_flags.get(role, []))}
master_home = {self.root}/work
master_stdout = {self.output}/{role}.stdout
master_stderr = {self.output}/{role}.stderr
app_use_limit = 0
app_idle_limit = 0
{application}}}
""",
            )
        user, group = pwd.getpwuid(os.getuid()).pw_name, grp.getgrgid(os.getgid()).gr_name
        write(
            self.root / "conf/main.cf",
            f"""owner_user = {user}
owner_group = {group}
master_owner = {user}
default_process_limit = 4
queue_directory = {self.root}/var
pid_file = {self.root}/var/pid/master.pid
log_file = {self.output}/master.log
service_directory = {self.root}/conf/service
service_file =
daemon_directory = {pathlib.Path(self.args.master).resolve().parent}
manage_addr = {self.management}
waiting_on_stop = true
stop_kill = true
service_throttle_time = 1
file_exts = .cf
scan_subdir = 0
""",
        )
        self.admin = common.Admin(
            f"{self.scheme}://127.0.0.1:{self.ports['server']}/admin", self.context
        )

    def rows(self, role):
        path = pathlib.Path(str(self.configs[role]) + ".metrics")
        if not path.exists():
            return []
        return [json.loads(line) for line in path.read_text().splitlines()]

    def ctl(self, role, action):
        response = subprocess.run(
            [self.args.ctl, "-s", self.management, "-f", str(self.configs[role]), "-a", action],
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
            timeout=20,
        )
        with (self.output / "ctl.log").open("a") as log:
            log.write(role + " " + action + "\n" + response.stdout)
        assert response.returncode == 0, response.stdout

    def start(self):
        if self.args.mode == "managed":
            self.master = subprocess.Popen(
                [
                    self.args.master,
                    "-c",
                    str(self.root / "conf"),
                    "-l",
                    str(self.output / "master.log"),
                ],
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
            )
        else:
            for role in self.roles:
                binary_role = "server" if role == "server" else "client"
                log = (self.output / (role + ".stderr")).open("wb")
                child = subprocess.Popen(
                    [
                        getattr(self.args, binary_role),
                        "-alone",
                        "-f",
                        str(self.configs[role]),
                        "-noise-min-bytes",
                        "256",
                    ]
                    + self.client_flags.get(role, []),
                    cwd=self.root / "work",
                    stdout=log,
                    stderr=log,
                )
                self.processes[role] = child, log
        eventually(lambda: self.admin.call("status")["setup_required"], 30)
        self.admin.call(
            "setup",
            {
                "username": "multi-admin",
                "password": "isolated-management-password",
                "setup_token": (self.root / "setup.token").read_text().strip(),
            },
        )
        eventually(self.admin.authenticated)
        eventually(lambda: all(self.rows(role) for role in self.roles), 20)
        for role in self.roles:
            self.pids[role] = self.rows(role)[-1]["pid"]
        runtime = self.admin.runtime()
        assert runtime["http_framework"] == "gin-service" and runtime["management_shared"]
        assert runtime["listener_owner"] == (
            "acl_master" if self.args.mode == "managed" else "sgate"
        )
        assert "noise-min-bytes" in runtime["cli_overrides"]
        eventually(lambda: self.rows("server")[-1]["census"]["attached"] == 3, 30)

    def census(self, phase, idle=True, connected=True):
        if idle:
            eventually(
                lambda: all(
                    not self.rows(role)[-1]["census"]["streams"]
                    and not self.rows(role)[-1]["census"]["pending_bytes"]
                    for role in self.roles
                )
                and not any(b.active() for b in self.backends.values()),
                35,
            )
        for config in self.configs.values():
            write(pathlib.Path(str(config) + ".collect"), phase)

        def ready():
            return all(any(row["phase"] == phase for row in self.rows(role)) for role in self.roles)

        eventually(ready, 15)
        result = {}
        for role in self.roles:
            row = next(row for row in reversed(self.rows(role)) if row["phase"] == phase)
            assert row["pid"] == self.pids[role] and not row["stack_truncated"]
            assert int(row["noise_min_bytes"]) == 256, "CLI override lost"
            if idle:
                assert row["census"]["streams"] == row["census"]["pending_bytes"] == 0
                assert row["transport_writers"] <= (3 if role == "server" else 1)
                if connected:
                    assert row["census"]["noise_sent"] > 0 and row["census"]["noise_received"] > 0
            result[role] = row
            self.samples.append(dict(role=role, **row))
        return result

    def expected(self, index):
        return [self.default, "B", "C"][index % 3]

    def connect(self, wave, index, count):
        marker = self.expected(index)
        connection = socket.create_connection(("127.0.0.1", self.ports[f"client{index % 3}"]), 5)
        try:
            connection.settimeout(35)
            connection.sendall(HELLO.pack(marker.encode(), wave, index, count))
            assert receive(connection, SIZE) == payload("down", marker, wave, index, 0), (
                "wrong first downstream",
                wave,
                index,
                marker,
            )
        except BaseException:
            connection.close()
            raise
        return connection

    def peak(self, phase):
        eventually(lambda: self.rows("server")[-1]["census"]["streams"] == 128, 20)
        rows = self.census(phase, idle=False)
        census = rows["server"]["census"]
        assert census["streams"] == 128
        for marker, count in [(self.default, 43), ("B", 43), ("C", 42)]:
            assert census["target_streams:" + self.targets[marker]] == count, (marker, census)
        return rows

    def wave(self, number):
        ready = threading.Barrier(129, timeout=30)
        errors, stop = [], threading.Event()
        before = self.relay.drops

        def stream(index):
            connection = self.connect(number, index, self.args.records)
            marker = self.expected(index)
            try:
                ready.wait()

                def send():
                    try:
                        for sequence in range(self.args.records):
                            connection.sendall(payload("up", marker, number, index, sequence))
                            time.sleep(0.01)
                        connection.sendall(
                            payload("up-end", marker, number, index, self.args.records)
                        )
                    except BaseException as error:
                        errors.append(repr(error))

                sender = threading.Thread(target=send)
                sender.start()
                try:
                    for sequence in range(1, self.args.records):
                        assert receive(connection, SIZE) == payload(
                            "down", marker, number, index, sequence
                        ), ("downstream loss/duplicate/cross-stream", number, index, sequence)
                    assert receive(connection, SIZE) == payload(
                        "down-end", marker, number, index, self.args.records
                    )
                    assert connection.recv(1) == b"", "extra downstream bytes"
                finally:
                    sender.join(35)
                    assert not sender.is_alive()
            finally:
                connection.close()

        def faults():
            while not stop.wait(0.65):
                self.relay.drop()

        fault = threading.Thread(target=faults)
        try:
            with concurrent.futures.ThreadPoolExecutor(max_workers=128) as pool:
                futures = [pool.submit(stream, index) for index in range(128)]
                self.peak(f"peak-{number}")
                ready.wait()
                fault.start()
                for future in futures:
                    future.result(timeout=45)
        finally:
            stop.set()
            if fault.ident:
                fault.join(5)
            ready.abort()
        assert not errors, errors
        assert not any(b.errors for b in self.backends.values()), [
            b.errors for b in self.backends.values()
        ]
        assert self.relay.drops > before, "wave injected no transport fault"
        self.waves.append(
            dict(
                number=number,
                connections=128,
                records=self.args.records,
                checked_duplex_bytes=128 * (self.args.records + 1) * SIZE * 2,
                transport_faults=self.relay.drops - before,
                default_target=self.default,
            )
        )

    def probe(self, query, status, sign_uri="", password="isolated-target-password"):
        value = dict(
            URL=f"{self.scheme}://127.0.0.1:{self.ports['server']}/tunnel" + query,
            Key=self.key,
            Password=password,
            CA=str(self.cert) if self.args.tls == "wss" else "",
            SignURI=sign_uri,
        )
        p = subprocess.run(
            [self.args.probe],
            input=json.dumps(value),
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            timeout=10,
        )
        assert p.returncode == 0, p.stderr
        assert json.loads(p.stdout)["status"] == status, (query, p.stdout, status)
        self.probes.append(dict(query=query, expected_status=status))

    def apply(self, edits, status=200):
        cfg = self.admin.call("config")
        generation = cfg["runtime"]["generation"]
        result = self.admin.call(
            "config",
            {"fields": edits, "revision": cfg["revision"], "apply": True},
            "PUT",
            status=status,
        )
        runtime = self.admin.runtime()
        assert runtime["pid"] == self.pids["server"]
        assert runtime["generation"] == generation + (1 if status == 200 else 0)
        if status != 200:
            assert (
                self.admin.call("config")["revision"] == cfg["revision"]
            ), "failed apply changed config"
        assert self.admin.authenticated()
        return result

    def hot_update(self):
        with concurrent.futures.ThreadPoolExecutor(max_workers=128) as pool:
            held = list(pool.map(lambda index: self.connect(99, index, 0), range(128)))
        try:
            self.peak("hot-update-peak")
            self.apply({"target": self.targets["D"], "allowed-targets": self.targets["C"]})
            self.default = "D"
            self.probe("?addr=" + urllib.parse.quote(self.targets["B"], safe=""), 403)
            self.probe("?addr=" + urllib.parse.quote(self.targets["D"], safe=""), 403)
            with concurrent.futures.ThreadPoolExecutor(max_workers=128) as pool:

                def closed(connection):
                    try:
                        return connection.recv(1) == b""
                    except ConnectionResetError:
                        return True

                assert all(
                    pool.map(closed, held)
                ), "old application connection survived apply/expiry"
        finally:
            for connection in held:
                connection.close()
        self.census("removed-target-idle", connected=False)
        self.apply({"allowed-targets": "127.0.0.1:0"}, status=400)
        self.apply({"allowed-targets": ",".join(self.targets.values())})
        eventually(lambda: self.rows("server")[-1]["census"]["attached"] == 3, 35)

    def snapshot(self, reason):
        for config in self.configs.values():
            write(pathlib.Path(str(config) + ".snapshot"), reason)
        time.sleep(0.5)

    def run(self):
        self.start()
        self.probe("?addr=" + urllib.parse.quote(self.targets["A"], safe=""), 403)
        self.probe("?addr=127.0.0.1:1", 403)
        self.probe("?addr=", 400)
        self.probe("?addr=localhost:0", 400)
        self.probe("?addr=http://localhost:80", 400)
        self.probe(
            "?addr="
            + urllib.parse.quote(self.targets["B"], safe="")
            + "&addr="
            + urllib.parse.quote(self.targets["C"], safe=""),
            400,
        )
        self.probe(
            "?addr=" + urllib.parse.quote(self.targets["C"], safe=""), 404, sign_uri="/tunnel"
        )
        self.probe("", 404, password="wrong-test-password")
        baseline = self.census("baseline")
        assert baseline["server"]["census"]["sessions"] == 3
        for number in range(self.args.rounds):
            self.wave(number)
            idle = self.census(f"idle-{number}")
            for role, row in idle.items():
                first = baseline[role]
                assert row["goroutines"] <= first["goroutines"] + 8, (
                    "goroutine growth",
                    role,
                    first,
                    row,
                )
                assert row["fds"] <= first["fds"] + 4, ("FD growth", role, first, row)
                assert row["heap_bytes"] <= first["heap_bytes"] + 16 * 1024 * 1024, (
                    "heap growth",
                    role,
                    first,
                    row,
                )
            if number == 0:
                self.hot_update()
        self.census("final-idle")
        return dict(
            status="passed",
            mode=self.args.mode,
            protocol=self.args.tls,
            concurrency=128,
            rounds=self.args.rounds,
            client_selection=self.args.selection,
            production_reconnect_backoff=True,
            process_restarts=0,
            process_pids=self.pids,
            waves=self.waves,
            probes=self.probes,
            samples=self.samples,
            upstream_records=sum(b.records for b in self.backends.values()),
            same_pid_hot_apply=True,
            old_connections_closed=True,
            failed_config_preserved=True,
        )

    def close(self):
        if self.args.mode == "managed" and self.master is not None:
            for role in reversed(self.roles):
                self.ctl(role, "stop")

            def gone():
                for pid in self.pids.values():
                    try:
                        os.kill(pid, 0)
                    except ProcessLookupError:
                        continue
                    return False
                return True

            eventually(gone, 20)
            self.master.send_signal(signal.SIGTERM)
            self.master.wait(15)
        else:
            for child, _ in self.processes.values():
                if child.poll() is None:
                    child.send_signal(signal.SIGINT)
            for child, log in self.processes.values():
                try:
                    child.wait(15)
                except subprocess.TimeoutExpired:
                    child.kill()
                    child.wait()
                    raise
                finally:
                    log.close()
                assert child.returncode == 0
        self.relay.close()
        for backend in self.backends.values():
            backend.close()
        for number in self.ports.values():
            with socket.socket() as probe:
                probe.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
                probe.bind(("127.0.0.1", number))
        closed = []
        for role, config in self.configs.items():
            for path in config.parent.glob(config.name + ".metrics*"):
                shutil.copy2(path, self.output / path.name)
            for row in self.rows(role):
                if row["phase"] == "closed":
                    assert row["transport_writers"] == 0 and row["goroutines"] <= 6, row
                    assert all(
                        row["census"][k] == 0
                        for k in [
                            "sessions",
                            "streams",
                            "pending_bytes",
                            "nonces",
                            "attached",
                            "registered_servers",
                        ]
                    ), row
                    closed.append(row)
        assert len(closed) == getattr(self, "expected_closes", 4), closed
        for path in self.output.glob("*.stderr"):
            assert "WARNING: DATA RACE" not in path.read_text(), path
        shutil.rmtree(self.root)
        return dict(
            ports_released=True, fixture_threads_released=True, closed_process_censuses=closed
        )


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--client", required=True)
    parser.add_argument("--server", required=True)
    parser.add_argument("--probe", required=True)
    parser.add_argument("--output", required=True)
    parser.add_argument("--mode", choices=["standalone", "managed"], required=True)
    parser.add_argument("--tls", choices=["ws", "wss"], required=True)
    parser.add_argument("--master", default="/opt/soft/acl-master/libexec/acl_master")
    parser.add_argument("--ctl", default="/opt/soft/acl-master/bin/master_ctl")
    parser.add_argument("--rounds", type=int, default=3)
    parser.add_argument("--records", type=int, default=128)
    parser.add_argument("--selection", choices=["url", "addr"], default="url")
    args = parser.parse_args()
    args.client = str(pathlib.Path(args.client).resolve())
    args.server = str(pathlib.Path(args.server).resolve())
    args.probe = str(pathlib.Path(args.probe).resolve())
    case = Case(args)
    result = {"status": "failed"}
    try:
        result = case.run()
    except BaseException as error:
        result["error"] = repr(error)
        case.snapshot(str(error))
        raise
    finally:
        try:
            result.update(case.close())
        finally:
            (case.output / "result.json").write_text(json.dumps(result, indent=2) + "\n")
    print(
        json.dumps(
            {
                key: value
                for key, value in result.items()
                if key not in ["samples", "closed_process_censuses", "probes"]
            },
            indent=2,
        )
    )


if __name__ == "__main__":
    main()
