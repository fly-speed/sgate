#!/usr/bin/env python3
"""One go-service client, three listener mappings; isolated WS/WSS and acl_master tests."""
import argparse
import importlib.util
import json
import pathlib
import signal
import socket
import subprocess
import time

ROOT = pathlib.Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("multi", ROOT / "scripts/test-multibackend.py")
multi = importlib.util.module_from_spec(spec)
spec.loader.exec_module(multi)


class Case(multi.Case):
    def setup(self):
        super().setup()
        self.mapping = ["A", "B", "C"]
        self.roles = ["server", "client0"]
        for role in ["client1", "client2"]:
            self.configs.pop(role).unlink()
        self.admin_port = multi.port()
        self.routes = [
            {
                "listen": f"127.0.0.1:{self.ports[f'client{i}']}",
                "server": f"{self.args.tls}://127.0.0.1:{self.relay.port}/tunnel"
                + ("" if i == 0 else "?addr=" + self.targets[marker]),
            }
            for i, marker in enumerate(self.mapping)
        ]
        path = self.configs["client0"]
        text = path.read_text()
        text = text.replace(
            f"master_service = 127.0.0.1|{self.ports['client0']}",
            "master_service = "
            + ",".join(f"127.0.0.1|{self.ports[f'client{i}']}" for i in range(3)),
        )
        settings = (
            f"proxies = '{json.dumps(self.routes)}'\nadmin_enabled = true\n"
            f"admin_web_dir = {ROOT}/web/admin\nadmin_listen = 127.0.0.1:{self.admin_port}\n"
            f"admin_users_file = {self.root}/client-users.json\n"
            f"admin_session_dir = {self.root}/client-sessions\n"
            f"admin_setup_token_file = {self.root}/client-setup.token\nadmin_session_timeout = 1h\n"
        )
        multi.write(path, text.replace("\n}", "\n" + settings + "}"))
        self.client_admin = multi.common.Admin(f"http://127.0.0.1:{self.admin_port}/admin", None)
        self.expected_closes = 2
        self.applies = 0

    def expected(self, index):
        return self.mapping[index % 3]

    def census(self, phase, idle=True, connected=True):
        def drained():
            return all(
                self.rows(role)
                and not self.rows(role)[-1]["census"]["streams"]
                and not self.rows(role)[-1]["census"]["pending_bytes"]
                for role in self.roles
            ) and not any(b.active() for b in self.backends.values())

        if idle:
            multi.eventually(drained, 35)
            multi.eventually(lambda: self.client_admin.runtime()["tcp_connections"] == 0, 20)
        for path in self.configs.values():
            multi.write(pathlib.Path(str(path) + ".collect"), phase)
        multi.eventually(
            lambda: all(any(r["phase"] == phase for r in self.rows(role)) for role in self.roles)
        )
        rows = {
            role: next(r for r in reversed(self.rows(role)) if r["phase"] == phase)
            for role in self.roles
        }
        for role, row in rows.items():
            assert row["transport_writers"] <= 3, row
            if idle:
                assert row["census"]["pending_bytes"] == row["census"]["streams"] == 0
            self.samples.append(dict(role=role, **row))
        rt = self.client_admin.runtime()
        assert rt["tcp_framework"] == "go-service" and rt["proxy_count"] == 3
        assert len(rt["listeners"]) == 3
        assert "noise-min-bytes" in rt["cli_overrides"]
        if not idle:
            assert rt["tcp_connections"] == 128, rt
        return rows

    def replace_routes(self, routes, status=200):
        def edit(text):
            return (
                "\n".join(
                    (
                        "proxies = '" + json.dumps(routes) + "'"
                        if line.strip().startswith("proxies =")
                        else line
                    )
                    for line in text.splitlines()
                )
                + "\n"
            )

        self.client_admin.apply(edit, status=status)
        if status == 200:
            self.applies += 1
        self.client_admin.authenticated()

    def restart_client(self):
        old = self.client_admin.runtime()["pid"]
        if self.args.mode == "managed":
            self.ctl("client0", "restart")
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
                ],
                cwd=self.root / "work",
                stdout=log,
                stderr=log,
            )
            self.processes["client0"] = child, log
        multi.eventually(
            lambda: self.client_admin.authenticated() and self.client_admin.runtime()["pid"] != old,
            30,
        )
        self.pids["client0"] = self.client_admin.runtime()["pid"]
        self.expected_closes += 1
        multi.eventually(
            lambda: self.rows("server")[-1]["census"]["attached"] == 3
            and self.rows("server")[-1]["census"]["sessions"] == 3,
            25,
        )

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
        self.wave(0)
        self.census("wave-0-idle")
        initial_pid = self.client_admin.runtime()["pid"]
        self.routes[0]["server"], self.routes[1]["server"] = (
            self.routes[1]["server"],
            self.routes[0]["server"],
        )
        self.mapping = ["B", "A", "C"]
        self.replace_routes(self.routes)
        assert self.client_admin.runtime()["pid"] == initial_pid
        self.wave(1)
        self.census("wave-1-idle")
        with socket.socket() as occupied:
            occupied.bind(("127.0.0.1", 0))
            occupied.listen()
            invalid = [dict(r) for r in self.routes]
            invalid[2]["listen"] = f"127.0.0.1:{occupied.getsockname()[1]}"
            before = self.configs["client0"].read_text()
            self.replace_routes(invalid, status=409)
            assert self.configs["client0"].read_text() == before
        self.restart_client()
        self.wave(2)
        self.census("restart-1-idle")
        self.restart_client()
        self.wave(3)
        final = self.census("final-idle")
        for role in self.roles:
            assert final[role]["goroutines"] <= baseline[role]["goroutines"] + 8
            assert final[role]["heap_bytes"] <= baseline[role]["heap_bytes"] + 12 * 1024 * 1024
        return dict(
            status="passed",
            mode=self.args.mode,
            protocol=self.args.tls,
            concurrency=128,
            listeners=3,
            client_processes=1,
            hot_applies=self.applies,
            client_restarts=2,
            login_persistent=True,
            failed_apply_rollback=True,
            waves=self.waves,
            samples=self.samples,
        )

    def close(self):
        result = super().close()
        with socket.socket() as probe:
            probe.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            probe.bind(("127.0.0.1", self.admin_port))
        return result


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
    args.client = str(pathlib.Path(args.client).resolve())
    args.server = str(pathlib.Path(args.server).resolve())
    case = Case(args)
    result = {"status": "failed"}
    try:
        result = case.run()
    finally:
        try:
            result.update(case.close())
        finally:
            (case.output / "result.json").write_text(json.dumps(result, indent=2) + "\n")
    print(
        json.dumps(
            {k: v for k, v in result.items() if k not in ["samples", "closed_process_censuses"]}
        )
    )


if __name__ == "__main__":
    main()
