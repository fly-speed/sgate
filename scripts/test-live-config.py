#!/usr/bin/env python3
"""Save live settings during 128 duplex streams without restarting either proxy."""
import argparse
import importlib.util
import json
import pathlib
import threading
import time

ROOT = pathlib.Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("noise", ROOT / "scripts/test-server-noise.py")
noise = importlib.util.module_from_spec(spec)
spec.loader.exec_module(noise)
multi = noise.multi


class Case(noise.Case):
    def setup(self):
        super().setup()
        for backend in self.backends.values():
            backend.strict_identities = True
            backend.defer_downstream = not self.args.preload_downstream
        path = self.configs["client0"]
        text = path.read_text()
        multi.write(
            path,
            text.replace(
                "\n}", "\nnoise-interval = 0\nreconnect-min = 250ms\nreconnect-max = 10s\n}"
            ),
        )

    def save(self, admin, fields, apply=False, status=200):
        config = admin.call("config")
        answer = admin.call(
            "config",
            {"fields": fields, "revision": config["revision"], "apply": apply},
            "PUT",
            status=status,
        )
        if status == 200:
            saved = {field["key"]: field["value"] for field in admin.call("config")["fields"]}
            for key, value in fields.items():
                assert saved[key] == value, ("file value differs from submitted value", key)
        return answer

    def census(self, phase, idle=True, connected=True):
        rows = super().census(phase, idle=idle, connected=connected)
        for row in rows.values():
            for key in ["noise_timers", "heartbeat_timers", "reconnect_timers"]:
                if key in row["census"]:
                    assert 0 <= row["census"][key] <= 3, (
                        "timer ownership grew beyond tunnel count",
                        phase,
                        key,
                        row,
                    )
        return rows

    def close(self):
        result = super().close()
        tracked = False
        for row in result["closed_process_censuses"]:
            census = row["census"]
            for key in ["noise_timers", "heartbeat_timers", "reconnect_timers"]:
                if key in census:
                    tracked = True
                    assert census[key] == 0, ("timer retained after shutdown", key, census)
        result["timer_ownership_counters_verified"] = tracked
        return result

    def run(self):
        started = time.monotonic()
        self.start()
        self.client_admin.call(
            "setup",
            {
                "username": "live-config-admin",
                "password": "isolated-client-management-password",
                "setup_token": (self.root / "client-setup.token").read_text().strip(),
            },
        )
        self.client_admin.authenticated()
        baseline = self.census("baseline")
        runtimes = {
            role: admin.runtime()
            for role, admin in [("server", self.admin), ("client0", self.client_admin)]
        }
        for role, admin in [("server", self.admin), ("client0", self.client_admin)]:
            fields = admin.call("config")["fields"]
            marked = {f["key"] for f in fields if f["validation"].get("live")}
            assert {
                "noise-interval-min",
                "noise-interval-max",
                "noise-min-bytes",
                "noise-max-bytes",
            } <= marked
            if role == "client0":
                assert {"reconnect-min", "reconnect-max"} <= marked
            answer = self.save(
                admin,
                {"noise-interval": "20ms", "noise-min-bytes": "1K", "noise-max-bytes": "4K"},
                True,
            )
            assert (
                answer["live_applied"] and not answer["applied"] and not answer["requires_restart"]
            ), answer
        multi.eventually(
            lambda: self.admin.call("stats")["noise_tx_bytes"] > 0
            and self.client_admin.call("stats")["noise_tx_bytes"] > 0
        )
        # 冷参数先存盘；随后的热参数不得把这个默认后端提前切换。
        answer = self.save(self.admin, {"target": self.targets["B"]})
        assert answer["requires_restart"]
        failures = []
        commits = []
        self.live_commits = commits
        stop = threading.Event()

        def update():
            try:
                index = 0
                while not stop.is_set() or index < 6:
                    mode = ["random", "fixed", "off", "random"][index % 4]
                    for role, admin in [("server", self.admin), ("client0", self.client_admin)]:
                        fields = {
                            "noise-interval": "25ms" if mode == "fixed" else "0",
                            "noise-interval-min": "10ms" if mode == "random" else "0",
                            "noise-interval-max": (
                                ["30ms", "60ms"][index % 2] if mode == "random" else "0"
                            ),
                            "noise-min-bytes": ["1K", "2K", "4K"][index % 3],
                            "noise-max-bytes": ["8K", "32K", "128K"][index % 3],
                        }
                        if role == "client0":
                            fields.update(
                                {
                                    "reconnect-min": ["100ms", "200ms", "300ms"][index % 3],
                                    "reconnect-max": ["1s", "2s"][index % 2],
                                }
                            )
                        answer = self.save(admin, fields)
                        assert answer["live_applied"] and not answer["applied"]
                        assert answer["requires_restart"] == (role == "server")
                        runtime = admin.runtime()
                        assert (
                            runtime["pid"] == runtimes[role]["pid"]
                            and runtime["generation"] == runtimes[role]["generation"]
                        ), runtime
                        assert runtime["noise"]["min_bytes"] == 256, "CLI priority lost"
                        assert runtime["noise"]["mode"] == mode, runtime
                        assert (
                            runtime["noise"]["max_bytes"] == [8192, 32768, 131072][index % 3]
                        ), runtime
                        commit = {
                            "role": role,
                            "fields": fields,
                            "pid": runtime["pid"],
                            "generation": runtime["generation"],
                            "noise": runtime["noise"],
                        }
                        commits.append(commit)
                        with (self.output / "live-commits.jsonl").open("a") as log:
                            log.write(json.dumps(commit) + "\n")
                    index += 1
                    stop.wait(0.03)
            except BaseException as error:
                failures.append(repr(error))

        updater = threading.Thread(target=update)
        updater.start()
        try:
            for number in range(self.args.live_waves):
                self.wave(number)
                self.census(f"live-idle-{number}")
                assert not failures, failures
                print(
                    f"online wave {number + 1}/{self.args.live_waves} passed; commits={len(commits)}",
                    flush=True,
                )
        finally:
            stop.set()
            updater.join(30)
        assert not updater.is_alive() and not failures, failures
        self.save(self.admin, {"target": self.targets["A"]})
        for role, admin in [("server", self.admin), ("client0", self.client_admin)]:
            original = self.configs[role].read_text()
            self.save(admin, {"noise-max-bytes": "11M"}, status=400)
            assert self.configs[role].read_text() == original
            self.save(
                admin, {"noise-interval": "0", "noise-interval-min": "0", "noise-interval-max": "0"}
            )
            admin.authenticated()
            assert not admin.call("config")["requires_restart"]
        # 允许已经开始写出的一个小帧完成；之后计数应保持稳定。
        time.sleep(0.3)
        sent = [a.call("stats")["noise_tx_bytes"] for a in [self.admin, self.client_admin]]
        time.sleep(0.3)
        assert sent == [a.call("stats")["noise_tx_bytes"] for a in [self.admin, self.client_admin]]
        self.wave(self.args.live_waves)
        final = self.census("final-idle")
        for role in self.roles:
            assert final[role]["goroutines"] <= baseline[role]["goroutines"] + 8
            assert final[role]["heap_bytes"] <= baseline[role]["heap_bytes"] + 16 * 1024 * 1024
        return dict(
            status="passed",
            elapsed_seconds=round(time.monotonic() - started, 3),
            live_waves=self.args.live_waves,
            fixture_synchronized_start=not self.args.preload_downstream,
            records_per_stream=self.args.records,
            mode=self.args.mode,
            protocol=self.args.tls,
            live_commits=len(commits),
            unchanged_pid_generation=True,
            cli_priority=True,
            pending_cold_config_isolated=True,
            noise_disabled=True,
            invalid_values_rollback=True,
            login_persistent=True,
            waves=self.waves,
            samples=self.samples,
        )


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ["client", "server", "output"]:
        parser.add_argument("--" + name, required=True)
    parser.add_argument("--mode", choices=["standalone", "managed"], required=True)
    parser.add_argument("--tls", choices=["ws", "wss"], required=True)
    parser.add_argument("--master", default="/opt/soft/acl-master/libexec/acl_master")
    parser.add_argument("--ctl", default="/opt/soft/acl-master/bin/master_ctl")
    parser.add_argument("--live-waves", type=int, default=1, help="连续在线修改期间的128并发波次数")
    parser.add_argument(
        "--preload-downstream",
        action="store_true",
        help="诊断旧夹具：屏障前预发下行；默认等待屏障后发送完整双向流",
    )
    parser.add_argument("--records", type=int, default=128, help="每连接每方向的记录数")
    args = parser.parse_args()
    if args.live_waves < 1 or args.records < 128:
        parser.error("live-waves 须至少为1，records 须至少为128")
    args.selection = "url"
    for name in ["client", "server"]:
        setattr(args, name, str(pathlib.Path(getattr(args, name)).resolve()))
    case, result = Case(args), {"status": "failed"}
    try:
        result = case.run()
    except BaseException as error:
        result.update(
            error=repr(error),
            waves=case.waves,
            samples=case.samples,
            live_commits=len(getattr(case, "live_commits", [])),
        )
        try:
            case.snapshot(str(error))
        except BaseException as snapshot_error:
            result["snapshot_error"] = repr(snapshot_error)
        raise
    finally:
        try:
            result.update(case.close())
        except BaseException as error:
            result.update(status="failed", cleanup_error=repr(error))
            raise
        finally:
            (case.output / "result.json").write_text(json.dumps(result, indent=2) + "\n")
    print(result["status"])


if __name__ == "__main__":
    main()
