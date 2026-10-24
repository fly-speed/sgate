#!/usr/bin/env python3
"""Real 120-second retention for one client process with three go-service listeners."""
import argparse
import contextlib
import importlib.util
import json
import pathlib
import time
import socket
import threading

ROOT = pathlib.Path(__file__).resolve().parents[1]


def load(name, filename):
    spec = importlib.util.spec_from_file_location(name, ROOT / "scripts" / filename)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


combined = load("combined", "test-client-multi-proxy.py")
retention = load("retention", "test-send-addr-retention.py")
multi = combined.multi


class Case(combined.Case):
    gate = retention.Case.gate
    wait_seconds = retention.Case.wait_seconds
    offline = retention.Case.offline
    online = retention.Case.online
    open_all = retention.Case.open_all
    queued = retention.Case.queued
    close_all = retention.Case.close_all
    check_eof = retention.Case.check_eof
    within_window = retention.Case.within_window

    def setup(self):
        for backend in self.backends.values():
            backend.close()
        self.backends = {key: retention.Backend(key) for key in "ABCD"}
        self.targets = {key: backend.address for key, backend in self.backends.items()}
        self.targets["C"] = "localhost:" + self.targets["C"].split(":")[1]
        super().setup()
        # 不写入保留时间或退避参数，实际使用生产默认值。
        for config in self.configs.values():
            lines = [
                line
                for line in config.read_text().splitlines()
                if not line.startswith(
                    ("session-ttl =", "offline-timeout =", "reconnect-min =", "reconnect-max =")
                )
            ]
            multi.write(config, "\n".join(lines) + "\n")
        self.events = []
        self.expiry_samples = []
        self.old_server_pid = None
        self.blocked = threading.Event()
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

    def initialize(self):
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
        assert baseline["server"]["census"]["session_ttl_ns"] == 120_000_000_000
        endpoints = baseline["client0"]["endpoints"]
        assert len(endpoints) == 3 and all(
            e["offline_timeout_ns"] == 120_000_000_000 for e in endpoints
        )
        assert baseline["client0"]["census"]["client_tunnels"] == 3
        return baseline

    def expiry_state(self, started):
        rows = {role: self.rows(role)[-1] for role in self.roles}
        sample = dict(seconds=time.monotonic() - started, rows=rows)
        self.expiry_samples.append(sample)
        return rows

    def expired_window(self):
        connections = self.open_all(81, 0)
        try:
            started = self.offline("beyond-window")
            self.queued(81, connections, 0)
            for at in [15, 60, 110]:
                self.wait_seconds(max(0, at - (time.monotonic() - started)))
                rows = self.expiry_state(started)
                assert rows["server"]["census"]["sessions"] == 3
                assert all(
                    row["census"]["streams"] == 128 and row["census"]["pending_bytes"] > 0
                    for row in rows.values()
                )
            released = {}
            while time.monotonic() - started < 135:
                rows = self.expiry_state(started)
                for role, row in rows.items():
                    if row["census"]["streams"] == row["census"]["pending_bytes"] == 0:
                        released.setdefault(role, time.monotonic() - started)
                if len(released) == 2:
                    break
                self.wait_seconds(0.2)
            assert len(released) == 2, ("expiry resource release timed out", rows)
            self.wait_seconds(max(0, 125 - (time.monotonic() - started)))
            self.check_eof(connections)
            rows = self.census("expired-idle", connected=False)
            assert rows["server"]["census"]["sessions"] == 0
            assert all(row["transport_writers"] == 0 for row in rows.values())
            self.events.append(
                dict(
                    event="expired-resources-released",
                    seconds=time.monotonic() - started,
                    release_seconds=released,
                )
            )
            self.online()
        finally:
            self.close_all(connections)
        self.wave(82)
        self.census("expired-recovered-idle")

    def restart_outage(self):
        connections = self.open_all(83, 0)
        try:
            self.offline("restart-outage")
            self.queued(83, connections, 0)
            retention.Case.restart_server(self)
            self.expected_closes = 3  # 同一客户端不重启，服务端两个进程。
            self.mapping = ["D", "B", "C"]
            self.wait_seconds(5)
            self.online()
            self.check_eof(connections)
        finally:
            self.close_all(connections)
        self.wave(84)
        self.census("restart-recovered-idle")

    def run(self):
        baseline = self.initialize()
        print("defaults verified: 120s, 128 streams, three tunnels", flush=True)
        self.within_window()
        print("75s outage: original TCP streams and both directions verified", flush=True)
        self.expired_window()
        print("125s outage: expired resources and recovery verified", flush=True)
        self.restart_outage()
        print("server restart: old streams released and new targets verified", flush=True)
        final = self.census("final-idle")
        assert final["client0"]["pid"] == baseline["client0"]["pid"]
        for role in self.roles:
            assert final[role]["goroutines"] <= baseline[role]["goroutines"] + 8
            assert final[role]["heap_bytes"] <= baseline[role]["heap_bytes"] + 16 * 1024 * 1024
        assert not any(b.errors for b in self.backends.values())
        return dict(
            status="passed",
            mode=self.args.mode,
            protocol=self.args.tls,
            concurrency=128,
            listeners=3,
            client_processes=1,
            retention_seconds=120,
            client_restarts=0,
            server_restarts=1,
            events=self.events,
            waves=self.waves,
            samples=self.samples,
            expiry_samples=self.expiry_samples,
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
    args.selection, args.records = "url", 128
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
        case.gate(True)
        try:
            result.update(case.close())
        finally:
            (case.output / "result.json").write_text(json.dumps(result, indent=2) + "\n")
    print(
        json.dumps(
            {
                k: v
                for k, v in result.items()
                if k not in ["samples", "closed_process_censuses", "expiry_samples"]
            }
        )
    )


if __name__ == "__main__":
    main()
