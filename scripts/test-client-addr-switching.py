#!/usr/bin/env python3
"""Exercise client addr precedence and hot switching with 128 real TCP streams."""

import argparse
import collections
import concurrent.futures
import contextlib
import importlib.util
import json
import pathlib
import socket
import time
import urllib.parse

spec = importlib.util.spec_from_file_location(
    "multi", pathlib.Path(__file__).with_name("test-multibackend.py")
)
multi = importlib.util.module_from_spec(spec)
spec.loader.exec_module(multi)


class Case(multi.Case):
    def setup(self):
        super().setup()
        self.choices = ["B", "B", "C"]
        self.admins = {"server": self.admin}
        self.transitions = []
        for index in range(3):
            role = f"client{index}"
            admin_port = multi.port()
            self.ports[role + "-admin"] = admin_port
            config = self.configs[role]
            text = config.read_text()
            # Start all three with a stale URL selection. File, CLI, and explicit
            # empty CLI select three different precedence branches.
            lines = [
                line
                for line in text.splitlines()
                if not line.startswith(("server =", "addr =", "master_args ="))
            ]
            self.client_flags[role] = (
                [] if index == 0 else ["-addr", self.targets["B"]] if index == 1 else ["-addr="]
            )
            if self.args.send_addr_toggle:
                if index != 0:
                    self.client_flags[role] += ["-send-addr=" + ("true" if index == 1 else "false")]
            settings = (
                f'master_args = -noise-min-bytes 256 {" ".join(self.client_flags[role])}\n'
                f'server = "{self.url("C")}"\naddr = {self.targets["B" if index != 1 else "A"]}\n'
                f"admin_enabled = true\nadmin_listen = 127.0.0.1:{admin_port}\n"
                f"admin_web_dir = {multi.ROOT}/web/admin\n"
                f"admin_users_file = {self.root}/{role}-users.json\n"
                f"admin_session_dir = {self.root}/{role}-sessions\n"
                f"admin_setup_token_file = {self.root}/{role}-setup.token\n"
                "admin_session_timeout = 1h\n"
            )
            if self.args.send_addr_toggle:
                settings += "send-addr = " + ("false" if index == 1 else "true") + "\n"
            if self.args.tls == "wss":
                settings += f"admin_tls_cert = {self.cert}\nadmin_tls_key = {self.root}/tls.key\n"
            multi.write(config, "\n".join(lines[:-1]) + "\n" + settings + "}\n")
            self.admins[role] = multi.common.Admin(
                f"{self.scheme}://127.0.0.1:{admin_port}", self.context
            )

        if self.args.send_addr_toggle:
            self.choices = ["B", "B", None]

    def url(self, marker=None):
        query = {"keep": "unchanged"}
        if marker:
            query["addr"] = self.targets[marker]
        return (
            f"{self.args.tls}://127.0.0.1:{self.relay.port}/tunnel?{urllib.parse.urlencode(query)}"
        )

    def start(self):
        super().start()
        for role, admin in self.admins.items():
            if role == "server":
                continue
            multi.eventually(lambda: admin.call("status")["setup_required"], 30)
            admin.call(
                "setup",
                {
                    "username": "switch-admin",
                    "password": "isolated-management-password",
                    "setup_token": (self.root / f"{role}-setup.token").read_text().strip(),
                },
            )
            multi.eventually(admin.authenticated)
            if self.client_flags[role]:
                assert "addr" in admin.runtime()["cli_overrides"], admin.runtime()
            if self.args.require_client_framework:
                runtime = admin.runtime()
                assert runtime["tcp_framework"] == "go-service", runtime
                assert runtime["listener_owner"] == (
                    "acl_master" if self.args.mode == "managed" else "sgate"
                )

    def census(self, phase, idle=True, connected=True):
        rows = super().census(phase, idle, connected)
        if idle and self.args.require_client_framework:
            multi.eventually(
                lambda: all(
                    self.admins[role].runtime()["tcp_connections"] == 0 for role in self.roles[1:]
                ),
                15,
            )
        return rows

    def expected(self, index):
        return self.choices[index % 3] or self.default

    def peak(self, phase):
        multi.eventually(lambda: self.rows("server")[-1]["census"]["streams"] == 128, 20)
        rows = self.census(phase, idle=False)
        census = rows["server"]["census"]
        expected = collections.Counter(self.expected(index) for index in range(128))
        for marker in self.targets:
            assert census.get("target_streams:" + self.targets[marker], 0) == expected[marker], (
                "target distribution",
                marker,
                expected,
                census,
            )
        return rows

    def apply_role(self, role, edits, status=200):
        admin = self.admins[role]
        cfg = admin.call("config")
        generation = cfg["runtime"]["generation"]
        admin.call(
            "config",
            {"fields": edits, "revision": cfg["revision"], "apply": True},
            "PUT",
            status=status,
        )
        runtime = admin.runtime()
        assert runtime["pid"] == self.pids[role]
        assert runtime["generation"] == generation + (status == 200)
        if status != 200:
            assert admin.call("config")["revision"] == cfg["revision"]
        assert admin.authenticated()

    def drained(self):
        # A new client worker uses a fresh session. Old detached sessions must
        # expire rather than accumulate across repeated applies.
        after = time.time_ns()

        def ready():
            row = self.rows("server")[-1]
            census = row["census"]
            return (
                row["unix_nano"] > after
                and census["sessions"] == 3
                and census["attached"] == 3
                and census["streams"] == 0
                and census["pending_bytes"] == 0
                and not any(backend.active() for backend in self.backends.values())
            )

        multi.eventually(ready, 35)

    def switch(self, label, edits, choices):
        with concurrent.futures.ThreadPoolExecutor(max_workers=128) as pool:
            held = list(
                pool.map(
                    lambda index: self.connect(100 + len(self.transitions), index, 0), range(128)
                )
            )
        try:
            self.peak(label + "-before")
            for role, fields in edits.items():
                self.apply_role(role, fields)
            for index, connection in enumerate(held):
                role = f"client{index % 3}"
                if role in edits or "server" in edits:
                    try:
                        assert connection.recv(1) == b"", "old stream survived worker replacement"
                    except ConnectionResetError:
                        pass
                else:
                    # Unchanged clients must retain their original streams.
                    connection.settimeout(0.02)
                    try:
                        value = connection.recv(1)
                    except socket.timeout:
                        continue
                    raise AssertionError(("unmodified client stream closed", role, value))
        finally:
            for connection in held:
                connection.close()
        self.choices = choices
        self.drained()
        rows = self.census(label + "-idle")
        self.transitions.append({"label": label, "selected": choices, "old_streams_closed": True})
        return rows

    def check_toggle_endpoint(self, label, enabled, selected, configured, rows=None):
        if rows is None:
            rows = self.census(label)
        for index in range(3):
            endpoints = rows[f"client{index}"]["endpoints"]
            assert len(endpoints) == 1, (label, index, endpoints)
            endpoint = endpoints[0]
            assert endpoint["omit_addr"] == (not enabled[index])
            assert endpoint["addr"] == (
                [self.targets[selected[index]]] if selected[index] else None
            )
            assert endpoint["configured_addr"] == configured[index]
            assert endpoint["path"] == "/tunnel" and endpoint["keep"] == "unchanged"
        return rows

    def run_toggle(self):
        self.start()
        baseline = self.check_toggle_endpoint(
            "baseline",
            [True, True, False],
            ["B", "B", None],
            [self.targets["B"], self.targets["B"], ""],
        )
        self.wave(0)
        # Repeatedly mutate the file values overridden by CLI as well. The
        # selection must remain stable for both explicit CLI true and false.
        stages = [
            ("disable", {"send-addr": "false"}, None, "B", None),
            ("restore-url", {"send-addr": "true", "addr": ""}, "C", "", None),
            ("disable-url", {"send-addr": "false"}, None, "", None),
            ("default-D", None, None, "", "D"),
            (
                "restore-independent",
                {"send-addr": "true", "addr": self.targets["B"]},
                "B",
                "B",
                None,
            ),
            ("disable-independent", {"send-addr": "false"}, None, "B", None),
            ("default-A", None, None, "B", "A"),
            ("restore-url-again", {"send-addr": "true", "addr": ""}, "C", "", None),
        ]
        for number, (label, fields, choice, configured, default) in enumerate(stages, 1):
            if default:
                edits = {"server": {"target": self.targets[default]}}
            else:
                edits = {
                    "client0": fields,
                    "client1": {"send-addr": "false" if number % 2 else "true"},
                    "client2": {"send-addr": "true" if number % 2 else "false"},
                }
            rows = self.switch(label, edits, [choice, "B", None])
            if default:
                self.default = default
            enabled = choice is not None
            self.check_toggle_endpoint(
                label + "-endpoint",
                [enabled, True, False],
                [choice, "B", None],
                [self.targets[configured] if configured else "", self.targets["B"], ""],
                rows=rows,
            )
            self.wave(number)
        self.apply_role("client0", {"send-addr": "invalid"}, status=400)
        final = self.census("final-idle")
        for role, row in final.items():
            first = baseline[role]
            assert row["goroutines"] <= first["goroutines"] + 8, (role, first, row)
            assert row["fds"] <= first["fds"] + 4, (role, first, row)
            assert row["heap_bytes"] <= first["heap_bytes"] + 16 * 1024 * 1024, (role, first, row)
        return dict(
            status="passed",
            scenario="send-addr",
            mode=self.args.mode,
            protocol=self.args.tls,
            concurrency=128,
            waves=self.waves,
            transitions=self.transitions,
            samples=self.samples,
            process_pids=self.pids,
            process_restarts=0,
            production_reconnect_backoff=True,
            upstream_records=sum(b.records for b in self.backends.values()),
        )

    def run(self):
        if self.args.send_addr_toggle:
            return self.run_toggle()
        self.start()
        self.probe("?addr=" + urllib.parse.quote(self.targets["A"], safe=""), 403)
        self.probe("?addr=127.0.0.1:1", 403)
        self.probe("?addr=", 400)
        self.probe("?addr=localhost:0", 400)
        self.probe(
            "?addr=" + urllib.parse.quote(self.targets["C"], safe=""), 404, sign_uri="/tunnel"
        )
        baseline = self.census("baseline")
        self.wave(0)
        # Empty file addr falls back to the original URL; nonempty/empty CLI
        # overrides stay effective even when the saved file changes.
        self.switch(
            "preserve-url",
            {
                "client0": {"addr": ""},
                "client1": {"addr": self.targets["D"], "server": self.url("C")},
                "client2": {"addr": self.targets["D"]},
            },
            ["C", "B", "C"],
        )
        self.wave(1)
        self.switch(
            "return-default",
            {
                "client0": {"addr": "", "server": self.url()},
                "client2": {"addr": self.targets["D"], "server": self.url()},
            },
            [None, "B", None],
        )
        self.wave(2)
        self.apply_role("client0", {"addr": "localhost:0"}, status=400)
        self.apply_role("client0", {"server": self.url() + "&addr="}, status=400)
        self.apply_role("server", {"allowed-targets": "localhost:0"}, status=400)
        # Valid syntax still cannot bypass the server allowlist. No backend
        # connection may be opened while the selected target is denied.
        self.apply_role("client0", {"addr": self.targets["D"]})
        with socket.create_connection(("127.0.0.1", self.ports["client0"]), 5) as connection:
            connection.settimeout(20)
            connection.sendall(b"denied target must never reach a backend")
            with contextlib.suppress(ConnectionResetError):
                assert connection.recv(1) == b""
        assert self.backends["D"].active() == 0 and not self.backends["D"].errors
        self.census("denied-idle", connected=False)
        self.apply_role("server", {"allowed-targets": ",".join(self.targets.values())})
        self.choices = ["D", "B", None]
        self.drained()
        self.wave(3)
        self.switch("clear-addr-after-allowlist", {"client0": {"addr": ""}}, [None, "B", None])
        self.switch(
            "server-default-update", {"server": {"target": self.targets["D"]}}, [None, "B", None]
        )
        self.default = "D"
        self.wave(4)
        final = self.census("final-idle")
        for role, row in final.items():
            first = baseline[role]
            assert row["goroutines"] <= first["goroutines"] + 8, (role, first, row)
            assert row["fds"] <= first["fds"] + 4, (role, first, row)
            assert row["heap_bytes"] <= first["heap_bytes"] + 16 * 1024 * 1024, (role, first, row)
        assert not any(backend.errors for backend in self.backends.values())
        return dict(
            status="passed",
            mode=self.args.mode,
            protocol=self.args.tls,
            concurrency=128,
            waves=self.waves,
            transitions=self.transitions,
            probes=self.probes,
            samples=self.samples,
            process_pids=self.pids,
            production_reconnect_backoff=True,
            process_restarts=0,
            upstream_records=sum(b.records for b in self.backends.values()),
        )


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ["client", "server", "probe", "output"]:
        parser.add_argument("--" + name, required=True)
    parser.add_argument("--mode", choices=["standalone", "managed"], required=True)
    parser.add_argument("--tls", choices=["ws", "wss"], required=True)
    parser.add_argument("--master", default="/opt/soft/acl-master/libexec/acl_master")
    parser.add_argument("--ctl", default="/opt/soft/acl-master/bin/master_ctl")
    parser.add_argument("--records", type=int, default=128)
    parser.add_argument("--send-addr-toggle", action="store_true")
    parser.add_argument("--require-client-framework", action="store_true")
    args = parser.parse_args()
    args.selection = "url"
    for name in ["client", "server", "probe"]:
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
        try:
            result.update(case.close())
        finally:
            (case.output / "result.json").write_text(json.dumps(result, indent=2) + "\n")
    print(
        json.dumps(
            {k: v for k, v in result.items() if k not in ["samples", "closed_process_censuses"]},
            indent=2,
        )
    )


if __name__ == "__main__":
    main()
