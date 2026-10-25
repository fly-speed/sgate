#!/usr/bin/env python3
"""Mapping form CRUD and restart lifecycle with 128 real streams in isolated services."""
import argparse
import collections
import importlib.util
import json
import pathlib
import signal
import socket
import subprocess
import threading
import time

ROOT = pathlib.Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location(
    "combined", ROOT / "scripts/test-client-multi-proxy.py"
)
combined = importlib.util.module_from_spec(spec)
spec.loader.exec_module(combined)
multi = combined.multi
LIVE = 0xFFFFFFFF


class Backend(multi.Backend):
    """Strict normal waves plus checked bidirectional prefixes during planned resets."""

    def __init__(self, marker, planned):
        self.planned = planned
        super().__init__(marker)

    def stream(self, connection):
        try:
            connection.settimeout(35)
            while True:
                hello = connection.recv(multi.HELLO.size, socket.MSG_PEEK)
                if not hello:
                    raise EOFError("closed before HELLO")
                if len(hello) == multi.HELLO.size:
                    break
                time.sleep(0.002)
            if multi.HELLO.unpack(hello)[-1] != LIVE:
                return super().stream(connection)
            marker, wave, index, _ = multi.HELLO.unpack(multi.receive(connection, multi.HELLO.size))
            assert marker.decode() == self.marker, ("wrong live backend", marker, self.marker)
            sequence = 0
            while not self.done.is_set():
                connection.sendall(multi.payload("down", self.marker, wave, index, sequence))
                want = multi.payload("up", self.marker, wave, index, sequence)
                got = bytearray()
                while len(got) < multi.SIZE:
                    chunk = connection.recv(multi.SIZE - len(got))
                    if not chunk:
                        assert got == want[: len(got)], "corrupt upstream prefix"
                        if not self.planned.is_set():
                            raise AssertionError("unplanned upstream EOF")
                        return
                    got.extend(chunk)
                    assert got == want[: len(got)], "corrupt upstream prefix"
                assert got == want, ("live upstream loss/duplicate/cross-stream", index, sequence)
                with self.lock:
                    self.records += 1
                sequence += 1
        except (OSError, EOFError) as error:
            if not self.done.is_set() and not self.planned.is_set():
                with self.lock:
                    self.errors.append(repr(error))
        except BaseException as error:
            if not self.done.is_set():
                with self.lock:
                    self.errors.append(repr(error))
        finally:
            connection.close()
            with self.lock:
                self.peers.discard(connection)


class LiveLoad:
    def __init__(self, case, number):
        self.case, self.number = case, number
        self.records = 0
        self.errors, self.ready = [], set()
        self.lock = threading.Lock()
        self.threads = [threading.Thread(target=self.stream, args=(i,)) for i in range(128)]
        case.planned.clear()
        for thread in self.threads:
            thread.start()
        multi.eventually(lambda: len(self.ready) == 128 and not self.errors, 25)
        case.peak("live-peak-" + str(number))

    def stream(self, index):
        case = self.case
        marker = case.expected(index)
        try:
            address = case.active[index % len(case.active)]["listen"]
            host, port = address.rsplit(":", 1)
            with socket.create_connection((host, int(port)), 5) as conn:
                conn.settimeout(20)
                conn.sendall(multi.HELLO.pack(marker.encode(), self.number, index, LIVE))
                sequence = 0
                while True:
                    conn.sendall(multi.payload("up", marker, self.number, index, sequence))
                    want = multi.payload("down", marker, self.number, index, sequence)
                    got = bytearray()
                    while len(got) < multi.SIZE:
                        chunk = conn.recv(multi.SIZE - len(got))
                        if not chunk:
                            assert got == want[: len(got)], "corrupt downstream prefix"
                            assert case.planned.is_set(), "unplanned downstream EOF"
                            return
                        got.extend(chunk)
                        assert got == want[: len(got)], "corrupt downstream prefix"
                    assert got == want, (
                        "live downstream loss/duplicate/cross-stream",
                        index,
                        sequence,
                    )
                    with self.lock:
                        self.ready.add(index)
                        self.records += 1
                    sequence += 1
                    time.sleep(0.02)
        except (OSError, EOFError) as error:
            if not case.planned.is_set():
                self.errors.append(repr(error))
        except BaseException as error:
            self.errors.append(repr(error))

    def alive(self):
        before = self.records
        multi.eventually(lambda: self.records > before + 128 and not self.errors, 5)
        assert all(t.is_alive() for t in self.threads) and not self.errors, self.errors

    def reset(self, action):
        self.case.planned.set()
        action()
        for thread in self.threads:
            thread.join(15)
            assert not thread.is_alive(), "old live stream did not close"
        assert not self.errors, self.errors
        self.case.live_records += self.records
        multi.eventually(lambda: not any(b.active() for b in self.case.backends.values()), 15)
        self.case.planned.clear()


class Case(combined.Case):
    def setup(self):
        for backend in self.backends.values():
            backend.close()
        self.planned = threading.Event()
        self.backends = {key: Backend(key, self.planned) for key in "ABCD"}
        self.targets = {key: b.address for key, b in self.backends.items()}
        self.targets["C"] = "localhost:" + self.targets["C"].split(":")[1]
        super().setup()
        self.ports["client3"] = multi.port()
        self.active = [dict(r) for r in self.routes]
        self.labels = ["A", "B", "C"]
        self.events, self.live_records, self.number = [], 0, 300
        self.cli_pinned = False
        server = self.configs["server"]
        multi.write(
            server,
            server.read_text().replace(
                f"allowed-targets = {self.targets['B']},{self.targets['C']}",
                f"allowed-targets = {self.targets['B']},{self.targets['C']},{self.targets['D']}",
            ),
        )
        config = self.configs["client0"]
        text = config.read_text().replace(
            "admin_session_timeout = 1h",
            f"admin_session_timeout = 1h\nadmin_master_ctl = {self.args.ctl}\nadmin_master_socket = {self.management}",
        )
        multi.write(config, text)
        self.password_line = next(
            line for line in text.splitlines() if line.startswith("password =")
        )

    def expected(self, index):
        return self.labels[index % len(self.labels)]

    def connect(self, wave, index, count):
        marker = self.expected(index)
        address = self.active[index % len(self.active)]["listen"]
        host, port = address.rsplit(":", 1)
        connection = socket.create_connection((host, int(port)), 5)
        try:
            connection.settimeout(35)
            connection.sendall(multi.HELLO.pack(marker.encode(), wave, index, count))
            assert multi.receive(connection, multi.SIZE) == multi.payload(
                "down", marker, wave, index, 0
            )
            return connection
        except BaseException:
            connection.close()
            raise

    def peak(self, phase):
        multi.eventually(lambda: self.rows("server")[-1]["census"]["streams"] == 128, 20)
        rows = self.census(phase, idle=False)
        counts = collections.Counter(self.expected(i) for i in range(128))
        for marker, count in counts.items():
            assert rows["server"]["census"]["target_streams:" + self.targets[marker]] == count
        return rows

    def census(self, phase, idle=True, connected=True):
        if idle:
            multi.eventually(
                lambda: all(
                    self.rows(role)
                    and self.rows(role)[-1]["census"]["streams"]
                    == self.rows(role)[-1]["census"]["pending_bytes"]
                    == 0
                    for role in self.roles
                )
                and not any(b.active() for b in self.backends.values()),
                35,
            )
            multi.eventually(lambda: self.client_admin.runtime()["tcp_connections"] == 0)
        for path in self.configs.values():
            multi.write(pathlib.Path(str(path) + ".collect"), phase)
        multi.eventually(
            lambda: all(
                any(row["phase"] == phase for row in self.rows(role)) for role in self.roles
            ),
            15,
        )
        rows = {
            role: next(r for r in reversed(self.rows(role)) if r["phase"] == phase)
            for role in self.roles
        }
        for role, row in rows.items():
            assert row["transport_writers"] <= len(self.active), row
            if idle:
                assert row["census"]["streams"] == row["census"]["pending_bytes"] == 0
            self.samples.append(dict(role=role, **row))
        rt = self.client_admin.runtime()
        assert rt["tcp_framework"] == "go-service" and rt["proxy_count"] == len(self.active), rt
        assert len(rt["listeners"]) == len(self.active)
        assert rows["client0"]["noise_min_bytes"] == "256", rows["client0"]
        if not idle:
            assert rt["tcp_connections"] == 128, rt
        return rows

    def submit(self, routes, apply=True, status=200, fields=None, revision=None):
        config = self.client_admin.call("config")
        answer = self.client_admin.call(
            "config",
            {
                "fields": fields or {},
                "proxies": routes,
                "revision": revision or config["revision"],
                "apply": apply,
            },
            "PUT",
            status=status,
        )
        self.client_admin.authenticated()
        text = self.configs["client0"].read_text()
        assert self.password_line in text, "stored password changed"
        assert "isolated-target-password" not in json.dumps(
            self.client_admin.call("config")
        ), "password leaked"
        return answer

    def ready_tunnels(self):
        multi.eventually(
            lambda: self.rows("server")[-1]["census"]["attached"] == len(self.active)
            and self.rows("server")[-1]["census"]["sessions"] == len(self.active),
            25,
        )

    def round(self, label):
        self.ready_tunnels()
        self.number += 1
        self.wave(self.number)
        self.census(label + "-idle")
        self.events.append(
            dict(event=label, ports=len(self.active), pid=self.client_admin.runtime()["pid"])
        )
        print(label, flush=True)

    def restart_saved(self):
        old = self.client_admin.runtime()["pid"]
        if self.args.mode == "managed":
            self.client_admin.call("restart-service", {"expected_pid": old}, "POST", status=202)
        else:
            child, log = self.processes["client0"]
            child.send_signal(signal.SIGINT)
            child.wait(20)
            assert child.returncode == 0
            log.close()
            log = (self.output / "client0.stderr").open("ab")
            child = subprocess.Popen(
                [
                    self.args.client,
                    "-alone",
                    "-f",
                    str(self.configs["client0"]),
                    "-noise-min-bytes",
                    "256",
                ]
                + self.client_flags.get("client0", []),
                cwd=self.root / "work",
                stdout=log,
                stderr=log,
            )
            self.processes["client0"] = child, log
        multi.eventually(
            lambda: self.client_admin.authenticated() and self.client_admin.runtime()["pid"] != old,
            35,
        )
        self.pids["client0"] = self.client_admin.runtime()["pid"]
        self.expected_closes += 1

    def change_ports(self, routes, labels, label):
        load = LiveLoad(self, self.number + 1000)
        if self.args.mode == "managed":
            before = self.configs["client0"].read_text()
            self.submit(routes, status=409)
            assert self.configs["client0"].read_text() == before
            load.alive()
            addresses = [r["listen"] for r in routes] or [f"127.0.0.1:{self.ports['client0']}"]
            service = ",".join(a.replace(":", "|") for a in addresses)
            self.submit(routes, apply=False, fields={"master_service": service})
            load.alive()
            load.reset(self.restart_saved)
        else:
            load.reset(lambda: self.submit(routes))
        self.active = [dict(r) for r in routes] or [
            {
                "listen": f"127.0.0.1:{self.ports['client0']}",
                "server": f"{self.args.tls}://127.0.0.1:{self.relay.port}/tunnel",
            }
        ]
        self.labels = labels
        self.round(label)

    def invalid_forms(self):
        load = LiveLoad(self, self.number + 1000)
        original = self.configs["client0"].read_text()
        generation = self.client_admin.runtime()["generation"]
        for bad in [
            self.routes + [dict(self.routes[0])],
            [dict(self.routes[0], server="http://localhost/tunnel")],
            [dict(self.routes[0], addr="not-a-port")],
        ]:
            self.submit(bad, status=400)
            assert self.configs["client0"].read_text() == original
            assert self.client_admin.runtime()["generation"] == generation
            load.alive()
        # Actual bind failure is a distinct rollback path; managed mode rejects port changes before stopping.
        with socket.socket() as occupied:
            occupied.bind(("127.0.0.1", 0))
            occupied.listen()
            bad = [dict(r) for r in self.routes]
            bad[0]["listen"] = f"127.0.0.1:{occupied.getsockname()[1]}"
            if self.args.mode == "standalone":
                load.reset(lambda: self.submit(bad, status=409))
            else:
                self.submit(bad, status=409)
                load.alive()
                load.reset(lambda: self.submit(self.routes))
        assert self.configs["client0"].read_text() == original
        self.round("invalid-and-bind-rollback")

    def pin_cli(self):
        # acl_master splits master_args on whitespace without shell unquoting.
        raw = json.dumps(self.active, separators=(",", ":"))
        self.client_flags["client0"] = ["-proxies", raw]
        config = self.client_admin.call("config")
        self.client_admin.call(
            "config",
            {
                "fields": {"master_args": "-noise-min-bytes 256 -proxies " + raw},
                "revision": config["revision"],
                "apply": False,
            },
            "PUT",
        )
        self.restart_saved()
        self.cli_pinned = True
        assert "proxies" in self.client_admin.runtime()["cli_overrides"]
        for index, saved in enumerate(
            [
                self.active + [dict(self.active[-1], listen=f"127.0.0.1:{self.ports['client3']}")],
                self.active[:1],
                [],
                [dict(r, addr=self.targets["B"]) for r in self.active],
            ]
        ):
            load = LiveLoad(self, self.number + 1000)
            load.reset(lambda: self.submit(saved, fields={"noise-min-bytes": "64"}))
            assert (
                next(
                    f
                    for f in self.client_admin.call("config")["fields"]
                    if f["key"] == "noise-min-bytes"
                )["value"]
                == "64"
            )
            self.round("cli-priority-" + str(index))

    def run(self):
        self.start()
        multi.eventually(lambda: self.client_admin.call("status")["setup_required"])
        self.client_admin.call(
            "setup",
            {
                "username": "multi-client-admin",
                "password": "isolated-client-management-password",
                "setup_token": (self.root / "client-setup.token").read_text().strip(),
            },
        )
        self.client_admin.authenticated()
        baseline = self.census("baseline")
        self.round("baseline")
        changed = next(
            field["mappings"]
            for field in self.client_admin.call("config")["fields"]
            if field["key"] == "proxies"
        )
        for key in ["server", "addr"]:
            changed[0][key], changed[1][key] = changed[1].get(key, ""), changed[0].get(key, "")
        load = LiveLoad(self, self.number + 1000)
        stale = self.client_admin.call("config")["revision"]
        self.submit(changed, apply=False)
        load.alive()
        self.submit(changed, apply=False, status=409, revision=stale)
        load.alive()
        load.reset(lambda: self.submit(changed))
        self.routes = changed
        self.active = [dict(r) for r in changed]
        self.labels = ["B", "A", "C"]
        self.round("modify-save-and-apply")
        self.invalid_forms()
        added = self.routes + [
            dict(
                listen=f"127.0.0.1:{self.ports['client3']}",
                server=f"{self.args.tls}://127.0.0.1:{self.relay.port}/tunnel",
                addr=self.targets["D"],
            )
        ]
        self.change_ports(added, ["B", "A", "C", "D"], "add")
        self.routes = added
        deleted = [added[0], added[2], added[3]]
        self.change_ports(deleted, ["B", "C", "D"], "delete")
        with socket.socket() as check:
            assert (
                check.connect_ex(("127.0.0.1", self.ports["client1"])) != 0
            ), "deleted port still listening"
        self.routes = deleted
        self.change_ports([], ["A"], "clear-to-single")
        restored = [
            dict(
                listen=f"127.0.0.1:{self.ports[f'client{i}']}",
                server=f"{self.args.tls}://127.0.0.1:{self.relay.port}/tunnel",
                **({"addr": self.targets[m]} if i else {}),
            )
            for i, m in enumerate(["A", "B", "C"])
        ]
        self.change_ports(restored, ["A", "B", "C"], "restore")
        self.routes = restored
        for index in range(3):
            load = LiveLoad(self, self.number + 1000)
            load.reset(
                lambda: self.submit(self.routes, fields={"noise-max-bytes": str(512 + index * 64)})
            )
            self.round("consecutive-apply-" + str(index))
        self.pin_cli()
        final = self.census("final-idle")
        for role in self.roles:
            assert final[role]["goroutines"] <= baseline[role]["goroutines"] + 8
            assert final[role]["heap_bytes"] <= baseline[role]["heap_bytes"] + 16 * 1024 * 1024
        assert not any(b.errors for b in self.backends.values())
        return dict(
            status="passed",
            mode=self.args.mode,
            protocol=self.args.tls,
            concurrency=128,
            events=self.events,
            waves=self.waves,
            samples=self.samples,
            checked_live_records=self.live_records,
            client_restarts=self.expected_closes - 2,
            password_preserved=True,
            persistent_login=True,
            cli_proxies_priority=True,
        )


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ["client", "server", "output"]:
        parser.add_argument("--" + name, required=True)
    parser.add_argument("--mode", choices=["standalone", "managed"], required=True)
    parser.add_argument("--tls", choices=["ws", "wss"], required=True)
    parser.add_argument("--master", default="/opt/soft/acl-master/libexec/acl_master")
    parser.add_argument("--ctl", default="/opt/soft/acl-master/bin/master_ctl")
    args = parser.parse_args()
    args.selection = "url"
    args.records = 128
    for name in ["client", "server"]:
        setattr(args, name, str(pathlib.Path(getattr(args, name)).resolve()))
    case = Case(args)
    result = {"status": "failed"}
    try:
        result = case.run()
    except BaseException as error:
        result["error"] = repr(error)
        case.snapshot(str(error))
        raise
    finally:
        case.planned.set()
        try:
            result.update(case.close())
        except BaseException as error:
            result.update(status="failed", cleanup_error=repr(error))
            raise
        finally:
            (case.output / "result.json").write_text(json.dumps(result, indent=2) + "\n")
    print(
        json.dumps(
            {k: v for k, v in result.items() if k not in ["samples", "closed_process_censuses"]}
        )
    )


if __name__ == "__main__":
    main()
