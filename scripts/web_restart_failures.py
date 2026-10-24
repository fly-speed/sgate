"""Fault injection for a private acl_master instance; never touches installed files."""

import collections
import concurrent.futures
import copy
import json
import os
import pathlib
import shlex
import threading
import time
import urllib.error
import urllib.request


class RestartChecks:
    def __init__(self, case, output, control, management, Admin, eventually):
        self.case = pathlib.Path(case)
        self.output = pathlib.Path(output)
        self.control = str(pathlib.Path(control).resolve())
        self.management = pathlib.Path(management)
        self.Admin, self.eventually = Admin, eventually
        self.results = []
        self.lock = threading.Lock()

    def wrapper(self, role):
        control = self.case / f"app/{role}-master_ctl"
        mode = self.case / f"app/{role}-control-mode"
        calls = self.case / f"app/{role}-control-pids"
        mode.write_text("normal")
        control.parent.mkdir(parents=True, exist_ok=True)
        control.write_text(
            "#!/bin/sh\n"
            f"printf '%s\\n' \"$$\" >> {shlex.quote(str(calls))}\n"
            f"mode=$(cat {shlex.quote(str(mode))})\n"
            'if [ "$mode" = hang ]; then exec /bin/sleep 120; fi\n'
            # Hold the first command so all requests can reach the old process.
            "sleep 1\n"
            f'exec {shlex.quote(self.control)} "$@"\n'
        )
        control.chmod(0o700)
        return str(control)

    def tool(self, role):
        return self.case / f"app/{role}-master_ctl"

    def pids(self, role):
        path = self.case / f"app/{role}-control-pids"
        return [int(value) for value in path.read_text().splitlines()] if path.exists() else []

    @staticmethod
    def gone(pid):
        try:
            os.kill(pid, 0)
            return False
        except ProcessLookupError:
            return True

    def post(self, original, expected_pid):
        # Each concurrent click has its own HTTP opener and a copy of the login cookie.
        client = self.Admin(original.base, original.context)
        for cookie in original.jar:
            client.jar.set_cookie(copy.copy(cookie))
        req = urllib.request.Request(
            client.base + "/api/restart-service",
            json.dumps({"expected_pid": expected_pid}).encode(),
            {"Content-Type": "application/json", "X-CSRF-Token": original.csrf},
            method="POST",
        )
        try:
            response = client.opener.open(req, timeout=10)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            body = json.load(response)
            if response.code == 202:
                assert body == {"restart_requested": True}, body
            else:
                assert response.code == 409 and "error" in body, (response.code, body)
            return response.code

    def batch(self, client, expected_pid, clicks=32):
        barrier = threading.Barrier(clicks)

        def click():
            barrier.wait(15)
            return self.post(client, expected_pid)

        with concurrent.futures.ThreadPoolExecutor(max_workers=clicks) as pool:
            return collections.Counter(pool.map(lambda _: click(), range(clicks)))

    def record(self, **result):
        with self.lock:
            self.results.append(dict(status="passed", **result))
            (self.output / "web-restart-checks.json").write_text(
                json.dumps(self.results, indent=2) + "\n"
            )

    def restart(self, role, admins, pid, pid_gone):
        client = admins[role]
        old = pid(role)
        before = len(self.pids(role))
        codes = self.batch(client, old)
        assert codes[202] == 1, ("duplicate accepted restart", role, codes)
        self.eventually(lambda: client.authenticated() and pid(role) != old, 25)
        self.eventually(lambda: pid_gone(old), 20)
        current = pid(role)
        # A delayed request from another browser must not restart the replacement.
        assert self.post(client, old) == 409
        time.sleep(0.1)
        assert pid(role) == current
        started = self.pids(role)[before:]
        assert len(started) == 1, ("multiple master_ctl processes", started)
        self.eventually(lambda: all(self.gone(value) for value in started), 10)
        self.record(
            role=role,
            scenario="concurrent_restart",
            clicks=sum(codes.values()),
            responses=dict(codes),
            old_pid=old,
            new_pid=current,
            stale_request_rejected=True,
            control_processes_released=True,
        )

    def failures(self, admins, pid, configs):
        for role in ["server", "client"]:
            for scenario in [
                "missing_command",
                "execute_permission",
                "missing_socket",
                "hung_command",
            ]:
                self.failure(role, scenario, admins[role], pid, configs[role])

    def failure(self, role, scenario, client, pid, config):
        old = pid(role)
        before = len(self.pids(role))
        content = pathlib.Path(config).read_bytes()
        mode = self.case / f"app/{role}-control-mode"
        tool = self.tool(role)
        hidden_tool = tool.with_suffix(".hidden")
        hidden_socket = self.management.with_suffix(".hidden")
        start = time.monotonic()
        try:
            if scenario == "missing_command":
                tool.rename(hidden_tool)
            elif scenario == "execute_permission":
                tool.chmod(0o600)
            elif scenario == "missing_socket":
                self.management.rename(hidden_socket)
            else:
                mode.write_text("hang")
            codes = self.batch(client, old)
            if scenario in ["missing_command", "execute_permission"]:
                assert codes == {409: 32}, codes
            else:
                assert codes[202] == 1 and codes[409] == 31, codes
                self.eventually(lambda: len(self.pids(role)) == before + 1, 5)
                self.eventually(lambda: self.gone(self.pids(role)[-1]), 50)
                if hidden_socket.exists():
                    hidden_socket.rename(self.management)
                self.eventually(lambda: not client.runtime()["service_restart_pending"], 35)
                assert "master_ctl" in client.runtime()["last_error"]
            assert not client.runtime()["service_restart_pending"]
            assert pid(role) == old, "failed command unexpectedly replaced the process"
            assert (
                pathlib.Path(config).read_bytes() == content
            ), "restart modified saved configuration"
            assert client.authenticated()
            assert "noise-min-bytes" in client.runtime()["cli_overrides"]
            started = self.pids(role)[before:]
            assert all(self.gone(value) for value in started), "control process retained"
            elapsed = time.monotonic() - start
            if scenario == "hung_command":
                assert 44 <= elapsed < 55, ("command execution deadline", elapsed)
            self.record(
                role=role,
                scenario=scenario,
                responses=dict(codes),
                seconds=elapsed,
                pid_unchanged=True,
                login_preserved=True,
                configuration_unchanged=True,
                cli_override_preserved=True,
                control_processes_released=True,
            )
            print(f"{self.output.name}/{role}: {scenario} passed ({elapsed:.1f}s)", flush=True)
        finally:
            if hidden_tool.exists():
                hidden_tool.rename(tool)
            tool.chmod(0o700)
            if hidden_socket.exists():
                hidden_socket.rename(self.management)
            mode.write_text("normal")
