#!/usr/bin/env python3
"""Serial macOS WSS stress comparison; never changes host network settings."""

import argparse
import datetime
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time


ROOT = Path(__file__).resolve().parents[1]


def command(*args):
    result = subprocess.run(args, capture_output=True, text=True, timeout=10)
    return result.stdout if result.returncode == 0 else None


def snapshot(phase, pid=None):
    raw = command("netstat", "-mmm")
    if not raw or "mbufs in use:" not in raw:
        raise RuntimeError("macOS netstat -mmm is unavailable")
    denied = re.search(r"(\d+) requests for memory denied", raw)
    pools = {
        size: {"in_use": int(used), "allocated": int(total)}
        for used, total, size in re.findall(r"(\d+)/(\d+) mbuf (\d+)KB clusters in use", raw)
    }
    failures = {}
    in_failures = False
    for line in raw.splitlines():
        if line.startswith("class"):
            in_failures = "failure" in line
        fields = line.split()
        if in_failures and len(fields) == 7 and fields[1].isdigit():
            failures[fields[0]] = int(fields[-1])
    memory = re.search(r"(\d+) KB allocated to network \(([\d.]+)% in use\)", raw)
    rss = command("ps", "-o", "rss=", "-p", str(pid)) if pid else None
    return {
        "time": datetime.datetime.now().astimezone().isoformat(),
        "monotonic": time.monotonic(),
        "phase": phase,
        "pid": pid,
        "rss_bytes": int(rss.strip()) * 1024 if rss and rss.strip().isdigit() else None,
        "network_memory_denied": int(denied[1]) if denied else 0,
        "pool_failures": failures,
        "cluster_pools_kb": pools,
        "network_allocated_bytes": int(memory[1]) * 1024 if memory else None,
        "network_in_use_percent": float(memory[2]) if memory else None,
        "raw_netstat": raw,
    }


def assert_report(report):
    assert report["tls"] and report["weak_network"]
    assert report["session_retention"] == "2m0s"
    assert report["verified_upload_bytes"] == report["verified_download_bytes"] == 3145728
    assert 119000 <= report["unconfirmed_release_milliseconds"] <= 125000
    closed = report["samples"][-1]
    assert closed["phase"] == "closed"
    for key in (
        "backend_connections",
        "local_connections",
        "http_connections",
        "sessions",
        "streams",
        "pending_bytes",
        "nonces",
        "client_sessions",
        "client_streams",
        "client_pending_bytes",
        "relay_connections",
        "relay_queued_bytes",
        "transport_writers",
    ):
        assert closed[key] == 0, (key, closed[key])
    assert closed["goroutines"] <= 6 and closed["file_descriptors"] <= 10


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, default=ROOT / "artifacts/network-memory-serial")
    parser.add_argument("--cooldown", type=int, default=10)
    args = parser.parse_args()
    if sys.platform != "darwin":
        parser.error("this diagnostic uses macOS netstat -mmm")
    if args.cooldown < 1:
        parser.error("--cooldown must be at least one second")
    out = args.output.resolve()
    out.mkdir(parents=True, exist_ok=True)
    env = {k: v for k, v in os.environ.items() if not k.startswith("SGATE_STRESS")}
    env.update(
        {
            "GOMAXPROCS": "4",
            "SGATE_STRESS": "1",
            "SGATE_STRESS_DUPLEX": "1",
            "SGATE_STRESS_TLS": "1",
            "SGATE_STRESS_WEAKNET": "1",
            "SGATE_STRESS_LIFECYCLE": "1",
            "SGATE_STRESS_DEFAULT_RETENTION": "1",
            "SGATE_STRESS_CLIENTS": "64",
            "SGATE_STRESS_STREAMS": "4",
            "SGATE_STRESS_ROUNDS": "2",
            "SGATE_STRESS_BYTES": "1024",
            "SGATE_STRESS_DURATION": "24s",
            "SGATE_STRESS_CHURN_INTERVAL": "1m",
            "SGATE_STRESS_WEAK_DELAY": "75ms",
            "SGATE_STRESS_WEAK_JITTER": "50ms",
            "SGATE_STRESS_WEAK_LOSS_PERCENT": "15",
            "SGATE_STRESS_WEAK_RETRANSMIT": "100ms",
            "SGATE_STRESS_NOISE_INTERVAL_MIN": "1s",
            "SGATE_STRESS_NOISE_INTERVAL_MAX": "3s",
        }
    )
    summary = {
        "serial": True,
        "environment": {
            k: v for k, v in env.items() if k.startswith("SGATE_STRESS") or k == "GOMAXPROCS"
        },
        "runs": [],
    }
    for mode in ("control", "fragmented"):
        for variant in ("plain", "race"):
            name = f"{mode}-{variant}"
            binary = ROOT / f"artifacts/network-memory-{variant}.test"
            if not binary.is_file():
                raise RuntimeError(f"First compile the {variant} test binary: {binary}")
            report_path = out / f"{name}.json"
            env.update(SGATE_STRESS_UNCONFIRMED=mode, SGATE_STRESS_REPORT=str(report_path))
            samples = []
            with (out / f"{name}-system.jsonl").open("w") as observations:

                def record(phase, pid=None):
                    item = snapshot(phase, pid)
                    samples.append(item)
                    observations.write(json.dumps(item) + "\n")
                    observations.flush()

                for _ in range(args.cooldown):
                    record("before")
                    time.sleep(1)
                print(f"Starting {name}; denied={samples[-1]['network_memory_denied']}", flush=True)
                with (out / f"{name}.log").open("w") as log:
                    child = subprocess.Popen(
                        [
                            str(binary),
                            "-test.run=^TestStressReconnectResources$",
                            "-test.v",
                            "-test.count=1",
                            "-test.timeout=6m",
                        ],
                        cwd=ROOT,
                        env=env,
                        stdout=log,
                        stderr=subprocess.STDOUT,
                    )
                    try:
                        while child.poll() is None:
                            record("running", child.pid)
                            time.sleep(1)
                    finally:
                        if child.poll() is None:
                            child.terminate()
                            try:
                                child.wait(timeout=10)
                            except subprocess.TimeoutExpired:
                                child.kill()
                                child.wait()
                    if child.returncode:
                        raise RuntimeError(f"{name} failed; inspect {out / (name + '.log')}")
                for _ in range(args.cooldown):
                    record("after")
                    time.sleep(1)
            report = json.loads(report_path.read_text())
            assert_report(report)
            first, last = samples[0], samples[-1]
            row = {
                "name": name,
                "network_memory_denied_delta": last["network_memory_denied"]
                - first["network_memory_denied"],
                "pool_failure_deltas": {
                    k: last["pool_failures"][k] - v for k, v in first["pool_failures"].items()
                },
                "peak_rss_bytes": max(s["rss_bytes"] or 0 for s in samples),
                "before_system": first,
                "after_system": last,
                "cluster_pool_peaks": {
                    size: {
                        key: max(s["cluster_pools_kb"][size][key] for s in samples)
                        for key in ("in_use", "allocated")
                    }
                    for size in first["cluster_pools_kb"]
                },
                "counter_changes": [
                    s
                    for a, s in zip(samples, samples[1:])
                    if s["network_memory_denied"] != a["network_memory_denied"]
                ],
                "closed_resources": report["samples"][-1],
                "release_milliseconds": report["unconfirmed_release_milliseconds"],
                "verified_upload_bytes": report["verified_upload_bytes"],
                "verified_download_bytes": report["verified_download_bytes"],
            }
            summary["runs"].append(row)
            (out / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
            print(
                f"Passed {name}; denied delta={row['network_memory_denied_delta']}; "
                f"peak RSS={row['peak_rss_bytes'] / 1048576:.1f} MiB",
                flush=True,
            )


if __name__ == "__main__":
    main()
