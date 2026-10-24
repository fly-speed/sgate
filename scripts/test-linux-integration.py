#!/usr/bin/env python3
"""Run real Linux deployment and 128-stream stress tests in isolated services.

Requires Go, Python, OpenSSL, ps, lsof, a C compiler for -race, and acl_master/master_ctl.
Never changes installed service configurations. Run from any working directory.
"""
import argparse
import datetime
import json
import os
import pathlib
import platform
import resource
import shutil
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parents[1]


def stress_command(common, mode, output, rounds):
    """两种传输都叠加管理请求；不能以部署冒烟测试替代 WS 压力测试。"""
    command = (
        [sys.executable, str(ROOT / "scripts/test-acl-master.py")]
        + common
        + [
            "--mode",
            mode,
            "--merged",
            "--stress",
            "--management-stress",
            "--tls",
            "both",
            "--concurrency",
            "128",
            "--rounds",
            str(rounds),
            "--output",
            str(output),
        ]
    )
    if mode == "managed":
        command.append("--web-service-restart")
    return command


def expected_results():
    """精确核对矩阵路径，避免重复结果或漏跑场景被总数量掩盖。"""
    expected = {}
    for build in ["plain", "race"]:
        for mode in ["standalone", "managed"]:
            for protocol in ["WS", "WSS"]:
                for paths in ["relative", "absolute"]:
                    key = f"{build}-deployment/{mode}-{protocol}-{paths}/result.json"
                    expected[key] = (mode, protocol == "WSS", False)
                prefix = f"{build}-{mode}-stress/"
                if mode == "standalone":
                    prefix += "standalone/"
                expected[f"{prefix}{protocol}/result.json"] = (mode, protocol == "WSS", True)
    return expected


def collect_results(output, rounds):
    expected = expected_results()
    actual = {str(path.relative_to(output)) for path in output.rglob("result.json")}
    if actual != expected.keys():
        raise RuntimeError(
            f"scenario matrix mismatch: missing={sorted(expected.keys() - actual)}, "
            f"unexpected={sorted(actual - expected.keys())}"
        )
    results = []
    for path, (mode, secure, stress) in expected.items():
        result = json.loads((output / path).read_text())
        if (
            result.get("status") != "passed"
            or result.get("mode") != mode
            or result.get("secure") != secure
        ):
            raise RuntimeError(f"failed or mismatched scenario: {path}")
        if stress:
            load = result.get("stress", {})
            management = load.get("management_load", [])
            if (
                load.get("concurrency") != 128
                or load.get("rounds") != rounds
                or len(management) != rounds
                or any(item.get("status") != "passed" for item in management)
                or not load.get("persistent_login")
                or not load.get("idle_resources_bounded")
                or not result.get("ports_released")
                or not result.get("closed_process_censuses", 0)
                or (mode == "managed" and not result.get("web_service_restart"))
            ):
                raise RuntimeError(f"incomplete stress verification: {path}")
        results.append({"path": path, "result": result})
    return results


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--master", default="/opt/soft/acl-master/libexec/acl_master")
    parser.add_argument("--ctl", default="/opt/soft/acl-master/bin/master_ctl")
    parser.add_argument("--output", default="artifacts/linux-integration")
    parser.add_argument("--rounds", type=int, default=6)
    args = parser.parse_args()
    if platform.system() != "Linux":
        parser.error("Linux runtime required; cross-compilation is not a runtime test")
    if args.rounds < 2:
        parser.error("at least two restart rounds are required to restart both roles")
    for name in ["go", "openssl", "ps", "lsof"]:
        if not shutil.which(name):
            parser.error(f"missing prerequisite: {name}")
    for name in ["master", "ctl"]:
        path = pathlib.Path(getattr(args, name)).resolve()
        if not path.is_file() or not os.access(path, os.X_OK):
            parser.error(f"{name} is not executable: {path}")
        setattr(args, name, str(path))
    soft, hard = resource.getrlimit(resource.RLIMIT_NOFILE)
    if soft != resource.RLIM_INFINITY and soft < 8192:
        if hard != resource.RLIM_INFINITY and hard < 8192:
            parser.error(
                "RLIMIT_NOFILE hard limit must be at least 8192; concurrency is not reduced"
            )
        resource.setrlimit(resource.RLIMIT_NOFILE, (8192, hard))
    output = pathlib.Path(args.output).resolve()
    if output.exists() and any(output.iterdir()):
        parser.error("use an empty output directory to keep each run auditable")
    output.mkdir(parents=True, exist_ok=True)
    env = os.environ.copy()
    env.setdefault("GOMAXPROCS", "4")
    env["CGO_ENABLED"] = "1"
    # Native Linux builds are required; inherited cross-build variables are unsafe.
    env.pop("GOOS", None)
    env.pop("GOARCH", None)
    report = {
        "status": "running",
        "started_utc": datetime.datetime.now(datetime.timezone.utc).isoformat(),
        "platform": dict(
            zip(["system", "node", "release", "version", "machine", "processor"], platform.uname())
        ),
        "go_version": subprocess.check_output(["go", "version"], env=env, text=True).strip(),
        "nofile": resource.getrlimit(resource.RLIMIT_NOFILE),
        "concurrency": 128,
        "rounds_per_mode": args.rounds,
        "commands": [],
        "results": [],
    }

    def save():
        (output / "verification.json").write_text(json.dumps(report, indent=2) + "\n")

    def run(name, command):
        entry = {"name": name, "argv": command, "status": "running"}
        report["commands"].append(entry)
        save()
        print(name, flush=True)
        with (output / (name + ".log")).open("w") as log:
            result = subprocess.run(
                command, cwd=ROOT, env=env, stdout=log, stderr=subprocess.STDOUT
            )
        entry.update(
            returncode=result.returncode, status="passed" if result.returncode == 0 else "failed"
        )
        save()
        if result.returncode:
            raise RuntimeError(f'{name} failed; see {output/(name+".log")}')

    try:
        for race in [False, True]:
            name = "race" if race else "plain"
            binaries = output / (name + "-binaries")
            run(
                name + "-build",
                [
                    sys.executable,
                    str(ROOT / "scripts/build-config-soak.py"),
                    "--managed",
                    "--output",
                    str(binaries),
                ]
                + (["--race"] if race else []),
            )
            suffix = "-race" if race else ""
            common = [
                "--client",
                str(binaries / ("sgate-client" + suffix)),
                "--server",
                str(binaries / ("sgate-server" + suffix)),
                "--master",
                args.master,
                "--ctl",
                args.ctl,
            ]
            run(
                name + "-deployment",
                [sys.executable, str(ROOT / "scripts/test-merged-deployment.py")]
                + common
                + ["--output", str(output / (name + "-deployment"))],
            )
            for mode in ["standalone", "managed"]:
                run(
                    name + "-" + mode + "-stress",
                    stress_command(
                        common, mode, output / (name + "-" + mode + "-stress"), args.rounds
                    ),
                )
        report["results"] = collect_results(output, args.rounds)
        report["status"] = "passed"
    except BaseException as error:
        report.update(status="failed", error=str(error))
        raise
    finally:
        report["finished_utc"] = datetime.datetime.now(datetime.timezone.utc).isoformat()
        save()
    print("Linux integration passed: " + str(output / "verification.json"))


if __name__ == "__main__":
    main()
