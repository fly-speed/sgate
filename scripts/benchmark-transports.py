#!/usr/bin/env python3
"""Transport matrix; fresh processes, bounded latency sampling, preserved failures.
Linux netem uses a private network namespace per sample, never the host qdisc.
"""
import argparse
import hashlib
import itertools
import json
import os
from pathlib import Path
import platform
import shutil
import signal
import statistics
import subprocess
import tempfile
import uuid


def integers(raw, minimum=0):
    values = [int(n) for n in raw.split(",")]
    if not values or any(n < minimum for n in values):
        raise ValueError("values outside allowed range")
    return values


def profile_matrix(args):
    cases = []
    for name in args.profiles.split(","):
        if name in ("loopback", "udp-loss"):
            cases.append((name, None))
        elif name in ("udp-matrix", "linux-netem"):
            for delay, loss, rate, mtu, reorder in itertools.product(
                integers(args.delays_ms), integers(args.loss_every), integers(args.rates_kbps),
                integers(args.mtu), integers(args.reorder_every),
            ):
                if delay > 2000 or rate > 1000000 or (mtu and not 1200 <= mtu <= 65000):
                    raise ValueError("profile outside safe ranges")
                if name == "linux-netem" and reorder and not delay:
                    raise ValueError("Linux reordering needs a positive delay")
                profile = dict(one_way_delay_ns=delay * 1000000, drop_every=loss,
                               bytes_per_second=rate * 1000 // 8, max_udp_payload=mtu,
                               reorder_every=reorder, reorder_delay_ns=8000000)
                cases.append((f"{name}-d{delay}-l{loss}-b{rate}-m{mtu}-r{reorder}", profile))
        else:
            raise ValueError("profiles: loopback,udp-loss,udp-matrix,linux-netem")
    return cases


def execute_sample(binary, env, log, namespace):
    created = False
    try:
        if namespace:
            subprocess.run(["ip", "netns", "add", namespace], check=True, stdout=log, stderr=subprocess.STDOUT)
            created = True
            subprocess.run(["ip", "-n", namespace, "link", "set", "lo", "up"], check=True)
        command = [str(binary), "-test.run=^TestTransportBaseline$", "-test.timeout=3m"]
        if namespace:
            command = ["ip", "netns", "exec", namespace] + command
        process = subprocess.Popen(command, env=env, stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
        try:
            return process.wait(timeout=190) == 0
        except subprocess.TimeoutExpired:
            os.killpg(process.pid, signal.SIGKILL)
            process.wait()
            return False
    finally:
        if namespace:
            # Also attempt cleanup after a partially failed namespace creation.
            cleanup = subprocess.run(["ip", "netns", "del", namespace], check=False,
                                     stdout=log, stderr=subprocess.STDOUT)
            if created and cleanup.returncode:
                raise subprocess.CalledProcessError(cleanup.returncode, cleanup.args)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--modes", default="wss,quic-v1,quic-v2")
    parser.add_argument("--profiles", default="loopback")
    parser.add_argument("--flows", default="1,8,32")
    parser.add_argument("--bytes", type=int, default=4 * 1024 * 1024)
    parser.add_argument("--repeats", type=int, default=3)
    parser.add_argument("--delays-ms", default="0,20")
    parser.add_argument("--loss-every", default="0,97")
    parser.add_argument("--rates-kbps", default="0,4096")
    parser.add_argument("--mtu", default="0", help="maximum UDP payload; Linux IP MTU is this plus 28")
    parser.add_argument("--reorder-every", default="0")
    args = parser.parse_args()
    try:
        flows = integers(args.flows, 1)
        cases = profile_matrix(args)
        if args.bytes <= 0 or args.repeats <= 0 or any(m not in ("wss", "quic-v1", "quic-v2") for m in args.modes.split(",")):
            raise ValueError("invalid bytes, repeats or mode")
    except ValueError as error:
        parser.error(str(error))
    linux = any(name.startswith("linux-netem") for name, _ in cases)
    if linux and (platform.system() != "Linux" or os.geteuid() != 0 or any(not shutil.which(c) for c in ("ip", "tc", "ethtool"))):
        parser.error("linux-netem requires Linux, root/CAP_NET_ADMIN+namespace privileges, ip, tc and ethtool; nothing was changed")
    root = Path(__file__).resolve().parents[1]
    os.chdir(root)
    args.output.mkdir(parents=True, exist_ok=False)
    results = []
    skipped = []
    with tempfile.TemporaryDirectory(prefix="sgate-baseline-") as tmp:
        binary = Path(tmp) / "tunnel.test"
        subprocess.run(["go", "test", "-mod=mod", "-c", "-o", str(binary), "./internal/tunnel"], check=True)
        fingerprint = hashlib.sha256()
        for source in sorted((root / "internal" / "tunnel").glob("*.go")):
            fingerprint.update(source.name.encode())
            fingerprint.update(source.read_bytes())
        manifest = dict(source_sha256=fingerprint.hexdigest(), binary_sha256=hashlib.sha256(binary.read_bytes()).hexdigest(),
                        host=platform.platform(), logical_cpus=os.cpu_count(), arguments=vars(args) | {"output": str(args.output)},
                        quantiles="nearest-rank", measurement="combined client/server/echo/generator; bulk + idle ping + bounded mixed-load ping; QUIC packet bytes exclude UDP/IP headers")
        (args.output / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
        for profile, spec in cases:
            for mode in args.modes.split(","):
                if mode == "wss" and profile.startswith("udp-"):
                    skipped.append(dict(mode=mode, network_profile=profile, reason="UDP relay cannot model TCP IP packet loss"))
                    continue
                for count in flows:
                    for repeat in range(args.repeats):
                        target = args.output.resolve() / f"{profile}-{mode}-{count}-{repeat}.json"
                        env = dict(os.environ, SGATE_BASELINE_OUTPUT=str(target), SGATE_BASELINE_MODE=mode,
                                   SGATE_BASELINE_FLOWS=str(count), SGATE_BASELINE_BYTES=str(args.bytes),
                                   SGATE_BASELINE_PROFILE=profile)
                        namespace = "sgate-bench-" + uuid.uuid4().hex[:12] if profile.startswith("linux-netem") else ""
                        env["SGATE_BASELINE_NETNS"] = namespace
                        if spec:
                            env["SGATE_BASELINE_IMPAIRMENT"] = json.dumps(spec)
                        with target.with_suffix(".log").open("w") as log:
                            try:
                                passed = execute_sample(binary, env, log, namespace)
                            except (OSError, subprocess.SubprocessError) as error:
                                log.write(f"sample setup/execution/cleanup failed: {error}\n")
                                passed = False
                        result = json.loads(target.read_text()) if target.exists() else dict(mode=mode, flows=count, network_profile=profile)
                        result["status"] = "passed" if passed else "failed"
                        if not passed:
                            result["failure_log"] = target.with_suffix(".log").name
                        target.write_text(json.dumps(result, indent=2) + "\n")
                        results.append(result)
    summary = []
    for profile, _ in cases:
        for mode in args.modes.split(","):
            for count in flows:
                rows = [r for r in results if (r["mode"], r["flows"], r["network_profile"]) == (mode, count, profile)]
                if not rows:
                    continue
                passed = [r for r in rows if r["status"] == "passed"]
                medians = {}
                for key in ("payload_mbps", "ping_p95_ms", "ping_p99_ms", "cpu_seconds", "allocated_bytes", "process_peak_rss_bytes",
                            "mixed_ping_p95_ms", "mixed_ping_p99_ms", "quic_retransmitted_stream_bytes", "quic_blocked_frames"):
                    values = [r[key] for r in passed if r.get(key) is not None]
                    medians[key] = statistics.median(values) if values else None
                summary.append(dict(mode=mode, network_profile=profile, flows=count, samples=len(rows), passed_samples=len(passed),
                                    failed_samples=len(rows)-len(passed), mixed_ping_timeouts=sum(r.get("mixed_ping_timeouts", 0) for r in passed),
                                    mixed_incomplete_samples=sum(not r.get("mixed_sampling_complete", False) for r in passed), **medians))
    (args.output / "skipped.json").write_text(json.dumps(skipped, indent=2) + "\n")
    (args.output / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
    print(json.dumps(summary, indent=2))
    return int(any(r["status"] == "failed" for r in results))


if __name__ == "__main__":
    raise SystemExit(main())
