#!/usr/bin/env python3
"""Real two-minute retention, send-addr hot changes, and server process restart."""

import argparse
import concurrent.futures
import contextlib
import importlib.util
import json
import pathlib
import signal
import socket
import subprocess
import threading
import time

spec = importlib.util.spec_from_file_location(
    "switching", pathlib.Path(__file__).with_name("test-client-addr-switching.py")
)
switching = importlib.util.module_from_spec(spec)
spec.loader.exec_module(switching)
multi = switching.multi


class Backend(multi.Backend):
    """Gate both normal data and disposable buffered data until transport is cut."""

    def __init__(self, marker):
        self.gate = threading.Event()
        self.gate.set()
        super().__init__(marker)

    def stream(self, connection):
        sender = None
        count = None
        errors = []
        try:
            connection.settimeout(230)
            marker, wave, index, count = multi.HELLO.unpack(
                multi.receive(connection, multi.HELLO.size)
            )
            assert marker.decode() == self.marker, (
                "wrong backend",
                marker,
                self.marker,
                wave,
                index,
            )
            connection.sendall(multi.payload("down", self.marker, wave, index, 0))
            assert self.gate.wait(200), "fixture data gate timed out"
            if count == 0:
                # These bytes must stay in the old session during outage and
                # be discarded on expiry/apply, never reach a replacement stream.
                connection.sendall(multi.payload("discard", self.marker, wave, index, 0) * 16)
                assert (
                    connection.recv(1) == b""
                ), "old offline stream unexpectedly delivered upstream"
                return

            def send():
                try:
                    for sequence in range(1, count):
                        connection.sendall(
                            multi.payload("down", self.marker, wave, index, sequence)
                        )
                    connection.sendall(multi.payload("down-end", self.marker, wave, index, count))
                except OSError as error:
                    errors.append(repr(error))

            sender = threading.Thread(target=send)
            sender.start()
            for sequence in range(count):
                assert multi.receive(connection, multi.SIZE) == multi.payload(
                    "up", self.marker, wave, index, sequence
                ), ("upstream integrity", self.marker, wave, index, sequence)
                with self.lock:
                    self.records += 1
            assert multi.receive(connection, multi.SIZE) == multi.payload(
                "up-end", self.marker, wave, index, count
            )
            sender.join(230)
            assert not sender.is_alive() and not errors, errors
        except (OSError, EOFError) as error:
            if count != 0 and not self.done.is_set():
                with self.lock:
                    self.errors.append(repr(error))
        except BaseException as error:
            if not self.done.is_set():
                with self.lock:
                    self.errors.append(repr(error))
        finally:
            with contextlib.suppress(OSError):
                connection.shutdown(socket.SHUT_RDWR)
            connection.close()
            if sender:
                sender.join(5)
            with self.lock:
                self.peers.discard(connection)


class Case(switching.Case):
    def setup(self):
        for backend in self.backends.values():
            backend.close()
        self.backends = {key: Backend(key) for key in "ABCD"}
        self.targets = {key: backend.address for key, backend in self.backends.items()}
        self.targets["C"] = "localhost:" + self.targets["C"].split(":")[1]
        super().setup()
        # Omit retention options entirely, so the production defaults are used.
        for role, config in self.configs.items():
            lines = [
                line
                for line in config.read_text().splitlines()
                if not line.startswith(("session-ttl =", "offline-timeout ="))
            ]
            if role == "client0":
                lines = ["addr =" if line.startswith("addr =") else line for line in lines]
            multi.write(config, "\n".join(lines) + "\n")
        self.choices = ["C", "B", None]
        self.events = []
        self.old_server_pid = None
        self.blocked = threading.Event()
        # Drop attempts before reaching the server, including TLS handshakes.
        original_copy = self.relay.copy

        def copy(source, dest):
            if self.blocked.is_set():
                for peer in [source, dest]:
                    with contextlib.suppress(OSError):
                        peer.shutdown(socket.SHUT_RDWR)
                    peer.close()
                    with self.relay.lock:
                        self.relay.peers.discard(peer)
                return
            original_copy(source, dest)

        self.relay.copy = copy

    def gate(self, enabled):
        for backend in self.backends.values():
            (backend.gate.set if enabled else backend.gate.clear)()

    def wait_seconds(self, duration):
        deadline = time.monotonic() + duration
        while time.monotonic() < deadline:
            assert not any(backend.errors for backend in self.backends.values()), [
                backend.errors for backend in self.backends.values()
            ]
            time.sleep(min(0.2, max(0, deadline - time.monotonic())))

    def offline(self, label):
        self.blocked.set()
        self.relay.drop()
        started = time.monotonic()
        # Newly accepted relays must not even open a server-side connection.
        # Redirect their target to a closed loopback port until restoration.
        self.relay.target = ("127.0.0.1", 0)
        self.relay.drop()
        after = time.time_ns()
        multi.eventually(
            lambda: self.rows("server")[-1]["unix_nano"] > after
            and self.rows("server")[-1]["census"]["attached"] == 0,
            15,
        )
        self.events.append(dict(event=label, disconnected_unix_nano=after))
        return started

    def online(self):
        self.relay.target = ("127.0.0.1", self.ports["server"])
        self.blocked.clear()
        after = time.time_ns()
        multi.eventually(
            lambda: self.rows("server")[-1]["unix_nano"] > after
            and self.rows("server")[-1]["census"]["attached"] == 3,
            40,
        )

    def open_all(self, wave, count):
        self.gate(False)
        with concurrent.futures.ThreadPoolExecutor(max_workers=128) as pool:
            connections = list(pool.map(lambda index: self.connect(wave, index, count), range(128)))
        for connection in connections:
            connection.settimeout(220)
        self.peak(f"hold-{wave}")
        return connections

    def queued(self, wave, connections, count):
        self.gate(True)
        for index, connection in enumerate(connections):
            marker = self.expected(index)
            if count:
                data = b"".join(
                    multi.payload("up", marker, wave, index, seq) for seq in range(count)
                )
                data += multi.payload("up-end", marker, wave, index, count)
            else:
                data = multi.payload("discard-up", marker, wave, index, 0) * 16
            connection.sendall(data)
        after = time.time_ns()
        multi.eventually(
            lambda: all(
                self.rows(role)[-1]["unix_nano"] > after
                and self.rows(role)[-1]["census"]["pending_bytes"] > 0
                for role in self.roles
            ),
            20,
        )
        return self.census(f"queued-{wave}", idle=False, connected=False)

    def close_all(self, connections):
        for connection in connections:
            connection.close()

    def check_eof(self, connections):
        def closed(connection):
            connection.settimeout(25)
            try:
                return connection.recv(1) == b""
            except ConnectionResetError:
                return True

        with concurrent.futures.ThreadPoolExecutor(max_workers=128) as pool:
            assert all(pool.map(closed, connections)), "expired old stream received bytes"

    def within_window(self):
        connections = self.open_all(80, 128)
        try:
            started = self.offline("within-window")
            self.queued(80, connections, 128)
            self.wait_seconds(max(0, 75 - (time.monotonic() - started)))
            before = self.census("before-resume", idle=False, connected=False)
            assert before["server"]["census"]["streams"] == 128
            assert before["server"]["census"]["sessions"] == 3
            self.online()

            def verify(pair):
                index, connection = pair
                marker = self.expected(index)
                connection.settimeout(40)
                for seq in range(1, 128):
                    assert multi.receive(connection, multi.SIZE) == multi.payload(
                        "down", marker, 80, index, seq
                    )
                assert multi.receive(connection, multi.SIZE) == multi.payload(
                    "down-end", marker, 80, index, 128
                )
                assert connection.recv(1) == b"", "duplicate downstream tail"

            with concurrent.futures.ThreadPoolExecutor(max_workers=128) as pool:
                list(pool.map(verify, enumerate(connections)))
            self.events.append(
                dict(
                    event="retained-data-verified",
                    outage_seconds=time.monotonic() - started,
                    connections=128,
                    checked_duplex_bytes=128 * 129 * 1024 * 2,
                )
            )
        finally:
            self.close_all(connections)
        self.census("within-idle")

    def expired_window(self):
        connections = self.open_all(81, 0)
        try:
            started = self.offline("beyond-window")
            self.queued(81, connections, 0)
            for at, fields in [
                (15, {"send-addr": "false"}),
                (30, {"send-addr": "true", "addr": self.targets["B"]}),
                (45, {"send-addr": "false"}),
                (60, {"send-addr": "true", "addr": ""}),
            ]:
                self.wait_seconds(max(0, at - (time.monotonic() - started)))
                self.apply_role("client0", fields)
                self.events.append(
                    dict(event="offline-apply", at=time.monotonic() - started, fields=fields)
                )
            self.wait_seconds(max(0, 125 - (time.monotonic() - started)))
            multi.eventually(
                lambda: self.rows("server")[-1]["census"]["sessions"] == 0
                and self.rows("server")[-1]["census"]["streams"] == 0,
                10,
            )
            self.check_eof(connections)
            rows = self.census("expired-idle", connected=False)
            assert rows["server"]["census"]["sessions"] == 0
            assert all(row["transport_writers"] == 0 for row in rows.values())
            self.events.append(
                dict(event="expired-resources-released", seconds=time.monotonic() - started)
            )
            self.choices = ["C", "B", None]
            self.online()
        finally:
            self.close_all(connections)
        self.wave(82)
        self.census("expired-recovered-idle")

    def restart_server(self):
        old_pid = self.pids["server"]
        self.old_server_pid = old_pid
        config = self.configs["server"]
        text = config.read_text().replace(
            "target = " + self.targets["A"], "target = " + self.targets["D"]
        )
        multi.write(config, text)
        if self.args.mode == "managed":
            self.ctl("server", "restart")
        else:
            child, log = self.processes["server"]
            child.send_signal(signal.SIGINT)
            child.wait(20)
            assert child.returncode == 0
            log.close()
            log = (self.output / "server.stderr").open("ab")
            child = subprocess.Popen(
                [self.args.server, "-alone", "-f", str(config), "-noise-min-bytes", "256"],
                cwd=self.root / "work",
                stdout=log,
                stderr=log,
            )
            self.processes["server"] = child, log
        multi.eventually(lambda: self.rows("server")[-1]["pid"] != old_pid, 30)
        self.pids["server"] = self.rows("server")[-1]["pid"]
        self.expected_closes = 5
        multi.eventually(self.admin.authenticated, 30)
        old = [
            row for row in self.rows("server") if row["pid"] == old_pid and row["phase"] == "closed"
        ]
        assert len(old) == 1 and old[0]["census"]["sessions"] == 0
        assert old[0]["transport_writers"] == 0 and old[0]["goroutines"] <= 6
        self.default = "D"
        self.events.append(
            dict(event="server-restarted", old_pid=old_pid, new_pid=self.pids["server"])
        )

    def restart_outage(self):
        connections = self.open_all(83, 0)
        try:
            self.offline("restart-outage")
            self.queued(83, connections, 0)
            self.apply_role("client0", {"send-addr": "false"})
            self.apply_role("client1", {"send-addr": "false"})
            self.apply_role("client2", {"send-addr": "true"})
            self.restart_server()
            self.wait_seconds(5)
            self.choices = [None, "B", None]
            self.online()
            self.check_eof(connections)
        finally:
            self.close_all(connections)
        self.wave(84)
        self.check_toggle_endpoint(
            "restart-final-idle",
            [False, True, False],
            [None, "B", None],
            ["", self.targets["B"], ""],
        )

    def run(self):
        self.start()
        baseline = self.check_toggle_endpoint(
            "baseline", [True, True, False], ["C", "B", None], ["", self.targets["B"], ""]
        )
        assert baseline["server"]["census"]["session_ttl_ns"] == 120_000_000_000
        for role in self.roles[1:]:
            assert baseline[role]["endpoints"][0]["offline_timeout_ns"] == 120_000_000_000
        self.within_window()
        self.expired_window()
        self.restart_outage()
        assert not any(backend.errors for backend in self.backends.values())
        final = self.census("final-idle")
        for role in self.roles[1:]:
            assert final[role]["pid"] == baseline[role]["pid"]
            assert final[role]["goroutines"] <= baseline[role]["goroutines"] + 8
            assert final[role]["heap_bytes"] <= baseline[role]["heap_bytes"] + 16 * 1024 * 1024
        return dict(
            status="passed",
            mode=self.args.mode,
            protocol=self.args.tls,
            concurrency=128,
            retention_seconds=120,
            events=self.events,
            waves=self.waves,
            samples=self.samples,
            process_pids=self.pids,
            old_server_pid=self.old_server_pid,
            upstream_records=sum(backend.records for backend in self.backends.values()),
        )


def main():
    p = argparse.ArgumentParser(description=__doc__)
    for key in ["client", "server", "probe", "output"]:
        p.add_argument("--" + key, required=True)
    p.add_argument("--mode", choices=["standalone", "managed"], required=True)
    p.add_argument("--tls", choices=["ws", "wss"], required=True)
    p.add_argument("--master", default="/opt/soft/acl-master/libexec/acl_master")
    p.add_argument("--ctl", default="/opt/soft/acl-master/bin/master_ctl")
    args = p.parse_args()
    args.send_addr_toggle, args.selection, args.records = True, "url", 128
    for key in ["client", "server", "probe"]:
        setattr(args, key, str(pathlib.Path(getattr(args, key)).resolve()))
    case = Case(args)
    result = {"status": "failed"}
    try:
        result = case.run()
    except BaseException as error:
        result["error"] = repr(error)
        case.snapshot(str(error))
        raise
    finally:
        case.gate(True)
        try:
            result.update(case.close())
        finally:
            (case.output / "result.json").write_text(json.dumps(result, indent=2) + "\n")
    print(
        json.dumps(
            {
                key: value
                for key, value in result.items()
                if key not in ["samples", "closed_process_censuses"]
            },
            indent=2,
        )
    )


if __name__ == "__main__":
    main()
