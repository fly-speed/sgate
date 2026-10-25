#!/usr/bin/env python3
"""Server noise settings applied through Web management, with 128 real TCP streams."""
import argparse
import importlib.util
import json
import pathlib
import time

ROOT = pathlib.Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location(
    "combined", ROOT / "scripts/test-client-multi-proxy.py"
)
combined = importlib.util.module_from_spec(spec)
spec.loader.exec_module(combined)
multi = combined.multi


class Case(combined.Case):
    def setup(self):
        super().setup()
        for role, path in self.configs.items():
            text = (
                path.read_text()
                .replace("noise-interval-min = 10ms", "noise-interval-min = 0")
                .replace("noise-interval-max = 30ms", "noise-interval-max = 0")
            )
            if role == "server":
                text = text.replace("noise-max-bytes = 512", "noise-max-bytes = 10M")
                text = text.replace("heartbeat = 1s", "noise-interval = 1h\nheartbeat = 1s")
            multi.write(path, text)

    def server_settings(self, fields, apply, status=200):
        config = self.admin.call("config")
        answer = self.admin.call(
            "config",
            {"fields": fields, "revision": config["revision"], "apply": apply},
            "PUT",
            status=status,
        )
        self.admin.authenticated()
        return answer

    def running_noise(self):
        return self.admin.runtime()["noise"]

    def ready_tunnels(self):
        # 应用配置会关闭旧会话；新业务须在三个新隧道全部建立后开始。
        multi.eventually(
            lambda: self.rows("server")[-1]["census"]["attached"] == 3
            and self.rows("server")[-1]["census"]["sessions"] == 3,
            20,
        )

    def wave(self, number):
        self.ready_tunnels()
        super().wave(number)

    def run(self):
        self.start()
        multi.eventually(lambda: self.client_admin.call("status")["setup_required"])
        self.client_admin.call(
            "setup",
            {
                "username": "noise-client-admin",
                "password": "isolated-client-management-password",
                "setup_token": (self.root / "client-setup.token").read_text().strip(),
            },
        )
        self.client_admin.authenticated()
        baseline = self.census("baseline")
        assert self.running_noise()["interval"] == "1h0m0s"
        assert self.running_noise()["max_bytes"] == 10 * 1024 * 1024
        generation = self.admin.runtime()["generation"]
        self.server_settings({"noise-interval": "300ms"}, False)
        assert self.running_noise()["interval"] == "300ms", "online setting was not applied"
        assert self.admin.runtime()["generation"] == generation, "online save restarted proxy"
        multi.eventually(
            lambda: self.admin.call("stats")["noise_tx_bytes"] > 0
            and self.client_admin.call("stats")["noise_rx_bytes"] > 0,
            20,
        )
        assert self.running_noise()["interval"] == "300ms"
        self.wave(0)
        self.census("fixed-idle")
        print("fixed interval and 10M limit passed", flush=True)
        self.server_settings(
            {
                "noise-interval": "0",
                "noise-interval-min": "200ms",
                "noise-interval-max": "400ms",
                "noise-min-bytes": "10M",
            },
            True,
        )
        running = self.running_noise()
        assert (
            running["mode"] == "random"
            and running["interval_min"] == "200ms"
            and running["interval_max"] == "400ms"
        ), running
        assert (
            running["min_bytes"] == 256 and running["max_bytes"] == 10485760
        ), "CLI/file size precedence changed"
        before = self.admin.call("stats")["noise_tx_bytes"]
        multi.eventually(lambda: self.admin.call("stats")["noise_tx_bytes"] > before, 20)
        self.wave(1)
        self.census("random-idle")
        print("random interval and CLI precedence passed", flush=True)
        self.server_settings({"noise-interval-min": "0", "noise-interval-max": "0"}, True)
        assert self.running_noise()["mode"] == "off"
        before = self.admin.call("stats")["noise_tx_bytes"]
        time.sleep(0.5)
        assert (
            self.admin.call("stats")["noise_tx_bytes"] == before
        ), "disabled server still sent noise"
        file_before = self.configs["server"].read_text()
        generation = self.admin.runtime()["generation"]
        self.server_settings({"noise-max-bytes": "11M"}, True, 400)
        assert self.configs["server"].read_text() == file_before
        assert self.admin.runtime()["generation"] == generation
        self.wave(2)
        final = self.census("final-idle")
        assert self.admin.call("stats")["noise_tx_bytes"] == before
        assert self.client_admin.call("stats")["noise_tx_bytes"] == 0
        assert self.admin.call("stats")["noise_rx_bytes"] == 0
        for role in self.roles:
            assert final[role]["goroutines"] <= baseline[role]["goroutines"] + 8
            assert final[role]["heap_bytes"] <= baseline[role]["heap_bytes"] + 16 * 1024 * 1024
        return dict(
            status="passed",
            mode=self.args.mode,
            protocol=self.args.tls,
            concurrency=128,
            waves=self.waves,
            samples=self.samples,
            server_noise_tx_bytes=before,
            client_noise_rx_bytes=self.client_admin.call("stats")["noise_rx_bytes"],
            save_only_applies_live=True,
            fixed_random_disabled=True,
            size_cli_priority=True,
            invalid_size_rollback=True,
            login_persistent=True,
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
    case, result = Case(args), {"status": "failed"}
    try:
        result = case.run()
    except BaseException as error:
        result["error"] = repr(error)
        case.snapshot(str(error))
        raise
    finally:
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
