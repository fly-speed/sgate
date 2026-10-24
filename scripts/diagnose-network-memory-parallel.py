#!/usr/bin/env python3
"""Run local WSS stress processes together; compare plain and race serially."""

import argparse
import collections
from concurrent.futures import ThreadPoolExecutor
import datetime
from decimal import Decimal
import importlib.util
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time


sys.dont_write_bytecode = True
ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location(
    "network_memory_serial", ROOT / "scripts/diagnose-network-memory.py"
)
serial = importlib.util.module_from_spec(spec)
spec.loader.exec_module(serial)


def timestamp():
    return datetime.datetime.now().astimezone().isoformat()


def optional_command(*args):
    """An inventory timeout is missing telemetry, never a reason to kill load."""
    try:
        return serial.command(*args)
    except (subprocess.TimeoutExpired, OSError):
        return None


def process_details(children):
    pids = [p.pid for p in children if p.poll() is None]
    result = {str(pid): {"rss_bytes": None, "tcp_descriptors": 0,
                         "tcp_states": {}, "receive_queue_bytes": 0,
                         "send_queue_bytes": 0, "listener_ports": []} for pid in pids}
    if not pids:
        return result
    selection = ",".join(map(str, pids))
    rss = optional_command("ps", "-o", "pid=,rss=", "-p", selection)
    for line in (rss or "").splitlines():
        fields = line.split()
        if len(fields) == 2 and fields[0] in result:
            result[fields[0]]["rss_bytes"] = int(fields[1]) * 1024
    raw = optional_command("lsof", "-nP", "-a", "-p", selection,
                         "-iTCP", "-Tqs", "-FpfntT")
    if raw is None:
        for row in result.values():
            row["tcp_descriptors"] = None
            row["tcp_telemetry_error"] = "lsof unavailable or timed out"
        return result
    pid, socket = None, None

    def finish_socket():
        if pid not in result or socket is None:
            return
        row = result[pid]
        row["tcp_descriptors"] += 1
        state = socket.get("state", "UNKNOWN")
        row["tcp_states"][state] = row["tcp_states"].get(state, 0) + 1
        row["receive_queue_bytes"] += socket.get("receive", 0)
        row["send_queue_bytes"] += socket.get("send", 0)
        if state == "LISTEN":
            match = re.search(r":(\d+)$", socket.get("name", ""))
            if match:
                row["listener_ports"].append(int(match[1]))

    for line in (raw or "").splitlines():
        if line.startswith("p"):
            finish_socket()
            pid, socket = line[1:], None
        elif line.startswith("f"):
            finish_socket()
            socket = {}
        elif socket is not None:
            if line.startswith("n"):
                socket["name"] = line[1:]
            elif line.startswith("TST="):
                socket["state"] = line[4:]
            elif line.startswith("TQR="):
                socket["receive"] = int(line[4:])
            elif line.startswith("TQS="):
                socket["send"] = int(line[4:])
    finish_socket()
    return result


def tcp_states(ports):
    raw = optional_command("netstat", "-an", "-p", "tcp")
    if raw is None:
        return {"unavailable": True}
    all_states, related = collections.Counter(), collections.Counter()
    loopback_ports = {host: set() for host in ("127.0.0.1", "::1")}
    ephemeral_states = {host: collections.defaultdict(set) for host in loopback_ports}
    for line in raw.splitlines():
        fields = line.split()
        if len(fields) < 6 or not fields[0].startswith("tcp"):
            continue
        all_states[fields[-1]] += 1
        endpoints = fields[3:5]
        host, _, port = fields[3].rpartition(".")
        if host in loopback_ports and port.isdigit():
            loopback_ports[host].add(int(port))
            if 49152 <= int(port) <= 65535:
                ephemeral_states[host][fields[-1]].add(int(port))
        if any((e.startswith("127.0.0.1.") or e.startswith("::1.")) and e.rsplit(".", 1)[-1].isdigit()
               and int(e.rsplit(".", 1)[-1]) in ports for e in endpoints):
            related[fields[-1]] += 1
    return {"system": dict(all_states), "test_listener_related": dict(related),
            "loopback_local_port_counts": {host: len(values) for host, values in loopback_ports.items()},
            "loopback_ephemeral_port_counts": {host: len({p for values in states.values() for p in values})
                                               for host, states in ephemeral_states.items()},
            "loopback_ephemeral_ports_by_state": {host: {state: len(values) for state, values in states.items()}
                                                  for host, states in ephemeral_states.items()},
            "ephemeral_range_counted": [49152, 65535]}


def duration_ns(value):
    """Compare Go duration spellings without treating 1m and 1m0s as different."""
    if value == "0":
        return 0
    units = {"ns": 1, "us": 1000, "µs": 1000, "μs": 1000,
             "ms": 10**6, "s": 10**9, "m": 60*10**9, "h": 3600*10**9}
    total, position = 0, 0
    for match in re.finditer(r"(\d+(?:\.\d*)?|\.\d+)(ns|us|µs|μs|ms|s|m|h)", value):
        if match.start() != position:
            raise ValueError(f"invalid duration: {value}")
        total += int(Decimal(match[1]) * units[match[2]])
        position = match.end()
    if not position or position != len(value):
        raise ValueError(f"invalid duration: {value}")
    return total


def validate(report, streams, retention_seconds=120, clients=64):
    assert report["tls"] and report["weak_network"] and report["independent_duplex"]
    assert report["clients"] == clients and report["streams_per_client"] == streams
    expected = f"{retention_seconds // 60}m{retention_seconds % 60}s" if retention_seconds >= 60 else f"{retention_seconds}s"
    assert report["session_retention"] == expected
    assert report["verified_upload_bytes"] > 0 and report["verified_download_bytes"] > 0
    if report.get("socket_buffer_bytes", 0) > 0 and "backend_sockets_bound" in report:
        assert report["backend_sockets_bound"] == clients*streams*3, "backend socket buffer bound did not cover all three cohorts"
    release = report["unconfirmed_release_milliseconds"]
    lower, upper = (retention_seconds - 1)*1000, (retention_seconds + 5)*1000
    assert lower <= release <= upper, f"session release {release}ms outside {lower}..{upper}ms"
    closed = report["samples"][-1]
    assert closed["phase"] == "closed"
    for key in (
        "backend_connections", "local_connections", "http_connections", "sessions",
        "streams", "pending_bytes", "nonces", "client_sessions", "client_streams",
        "client_pending_bytes", "relay_connections", "relay_queued_bytes", "transport_writers",
    ):
        assert closed[key] == 0, (key, closed[key])
    assert closed["goroutines"] <= 6 and closed["file_descriptors"] <= 10


def workloads(processes):
    """Distinct files are required even when multiple processes use the same mode."""
    if processes == 2:
        return [("control", "control"), ("fragmented", "fragmented")]
    return [(f"{mode}-{i // 2 + 1}", mode)
            for i, mode in enumerate(["control", "fragmented"] * (processes // 2))]


def capture_vmmap(pid, path):
    """Optional VM inspection must not pause mbuf event sampling."""
    started = timestamp()
    try:
        result = subprocess.run(["vmmap", "-summary", str(pid)],
                                capture_output=True, text=True, timeout=5)
        body = result.stdout + result.stderr
    except (subprocess.TimeoutExpired, OSError) as exc:
        body = f"vmmap unavailable for test PID {pid}: {exc}\n"
    path.write_text(f"Started: {started}\nCompleted: {timestamp()}\n{body}")


def release_load_barriers(directory, labels):
    released = []
    for round_number in (1, 2):
        start = directory / f"round-{round_number}.start"
        if not start.exists() and all((directory / f"{label}-round-{round_number}.ready").is_file() for label in labels):
            start.write_text(timestamp() + "\n")
            released.append(round_number)
    return released


def run_group(args, variant, out):
    binary = ROOT / f"artifacts/network-memory-parallel-{variant}.test"
    if not binary.is_file():
        raise RuntimeError(f"Compile {binary} first")
    env = {k: v for k, v in os.environ.items() if not k.startswith("SGATE_STRESS")}
    env.update({
        "GOMAXPROCS": "4", "SGATE_STRESS": "1", "SGATE_STRESS_DUPLEX": "1",
        "SGATE_STRESS_TLS": "1", "SGATE_STRESS_WEAKNET": "1",
        "SGATE_STRESS_LIFECYCLE": "1", "SGATE_STRESS_DEFAULT_RETENTION": "1",
        "SGATE_STRESS_SOCKET_BUFFER": str(args.socket_buffer),
        "SGATE_STRESS_RECONNECT_MIN": args.reconnect_min,
        "SGATE_STRESS_RECONNECT_MAX": args.reconnect_max,
        "SGATE_STRESS_CLIENTS": str(args.clients), "SGATE_STRESS_STREAMS": str(args.streams),
        "SGATE_STRESS_BACKEND_HOST": args.backend_host,
        "SGATE_STRESS_SERVER_HOST": args.server_host,
        "SGATE_STRESS_COHORT_SETTLE": args.cohort_settle,
        "SGATE_STRESS_ROUNDS": "2", "SGATE_STRESS_BYTES": "1024",
        "SGATE_STRESS_DURATION": "24s", "SGATE_STRESS_CHURN_INTERVAL": args.churn_interval,
        "SGATE_STRESS_WEAK_DELAY": "75ms", "SGATE_STRESS_WEAK_JITTER": "50ms",
        "SGATE_STRESS_WEAK_LOSS_PERCENT": "15", "SGATE_STRESS_WEAK_RETRANSMIT": "100ms",
        "SGATE_STRESS_NOISE_INTERVAL_MIN": "1s", "SGATE_STRESS_NOISE_INTERVAL_MAX": "3s",
    })
    if args.retention_seconds != 120:
        env["SGATE_STRESS_UNCONFIRMED_TTL"] = f"{args.retention_seconds}s"
    if args.trace:
        env["SGATE_STRESS_PROBE_TRACE"] = "1"
    if args.tunnel_log:
        env["SGATE_STRESS_LOG"] = "1"
    children, handles, observations, events = [], [], [], []
    labels, ports = {}, set()
    detail, detail_at = {}, 0.0
    previous, first_event = None, True
    cases = workloads(args.processes)
    barrier_dir = out / f"load-sync-{variant}"
    if args.synchronize_load:
        barrier_dir.mkdir(exist_ok=True)
        for round_number in (1, 2):
            (barrier_dir / f"round-{round_number}.start").unlink(missing_ok=True)
            for label, _ in cases:
                (barrier_dir / f"{label}-round-{round_number}.ready").unlink(missing_ok=True)
    reports = [out / f"{variant}-{label}.json" for label, _ in cases]
    live = [out / f"{variant}-{label}-live.json" for label, _ in cases]
    for path in reports + live:
        path.unlink(missing_ok=True)

    with ThreadPoolExecutor(max_workers=args.processes) as inspections, (out / f"{variant}-system.jsonl").open("w") as records:
        def record(phase, force=False):
            nonlocal detail, detail_at, previous, first_event
            item = serial.snapshot(phase)
            changed = previous is not None and (
                item["network_memory_denied"] != previous["network_memory_denied"]
                or item["pool_failures"] != previous["pool_failures"]
            )
            if not detail or force or changed or time.monotonic() - detail_at >= 1:
                detail = {"captured_at": timestamp(), "processes": process_details(children)}
                detail_at = time.monotonic()
                for row in detail["processes"].values():
                    ports.update(row["listener_ports"])
                detail["tcp_states"] = tcp_states(ports)
            item["details"] = detail
            item["detail_age_seconds"] = time.monotonic() - detail_at
            item["live_samples"] = {}
            for path in live:
                try:
                    sample = json.loads(path.read_text())
                    item["live_samples"][path.stem] = sample
                except FileNotFoundError:
                    pass
            if changed:
                event = {
                    "time": item["time"], "before": previous, "after": item,
                    "denied_delta": item["network_memory_denied"] - previous["network_memory_denied"],
                    "pool_failure_deltas": {k: item["pool_failures"][k] - v
                                            for k, v in previous["pool_failures"].items()},
                    "vm_stat": optional_command("vm_stat"),
                }
                events.append(event)
                (out / f"{variant}-events.json").write_text(json.dumps(events, indent=2) + "\n")
                print(f"EVENT {variant}: denied +{event['denied_delta']}; "
                      f"pools={event['pool_failure_deltas']}", flush=True)
                active = [child for child in children if child.poll() is None]
                if first_event and active:
                    first_event = False
                    for child in active:
                        inspections.submit(capture_vmmap, child.pid,
                                           out / f"{variant}-event-{child.pid}-vmmap.txt")
            records.write(json.dumps(item) + "\n")
            records.flush()
            observations.append(item)
            previous = item

        def observe_for(seconds, phase):
            until = time.monotonic() + seconds
            while time.monotonic() < until:
                record(phase)
                time.sleep(args.interval)

        try:
            observe_for(args.before, "before")
            group_start = time.monotonic()
            print(f"Starting parallel {variant}; {args.processes} x {args.clients} sessions x {args.streams} streams; "
                  f"denied={previous['network_memory_denied']}", flush=True)
            for index, ((label, mode), report, sample) in enumerate(zip(cases, reports, live)):
                if children:
                    while time.monotonic() - group_start < index * args.start_offset:
                        record("running")
                        time.sleep(args.interval)
                child_env = env | {"SGATE_STRESS_UNCONFIRMED": mode,
                                   "SGATE_STRESS_REPORT": str(report),
                                   "SGATE_STRESS_LIVE_REPORT": str(sample)}
                if args.synchronize_load:
                    child_env["SGATE_STRESS_LOAD_BARRIER_PREFIX"] = str(barrier_dir / label)
                handle = (out / f"{variant}-{label}.log").open("w")
                handles.append(handle)
                child = subprocess.Popen(
                    [str(binary), "-test.run=^TestStressReconnectResources$", "-test.v",
                     "-test.count=1", "-test.timeout=6m"], cwd=ROOT, env=child_env,
                    stdout=handle, stderr=subprocess.STDOUT,
                )
                children.append(child)
                labels[str(child.pid)] = label
            failure_captured = False
            while any(child.poll() is None for child in children):
                if args.synchronize_load:
                    for round_number in release_load_barriers(barrier_dir, [label for label, _ in cases]):
                        print(f"Full-load barrier released {variant} round {round_number}; {args.processes*args.clients*args.streams} application streams", flush=True)
                if not failure_captured and any(child.poll() not in (None, 0) for child in children):
                    failure_captured = True
                    record("running", force=True)
                    (out / f"{variant}-first-failure-tcp.txt").write_text(optional_command("netstat", "-anv", "-p", "tcp") or "unavailable")
                    (out / f"{variant}-first-failure-vm.txt").write_text(optional_command("vm_stat") or "unavailable")
                record("running")
                time.sleep(args.interval)
        finally:
            for child in children:
                if child.poll() is None:
                    child.terminate()
                    try:
                        child.wait(timeout=10)
                    except subprocess.TimeoutExpired:
                        child.kill()
                        child.wait()
            for handle in handles:
                handle.close()
        observe_for(args.after, "after")
        record("after", force=True)
    exits = {label: child.returncode for (label, _), child in zip(cases, children)}
    (out / f"{variant}-exit-codes.json").write_text(json.dumps(exits, indent=2) + "\n")
    if any(exits.values()):
        raise RuntimeError(f"{variant} stress failed: {exits}; inspect its logs")
    return summarize_group(args, variant, out, observations, labels, events)


def summarize_group(args, variant, out, observations, labels, events):
    """Warmup can have no processes; missing live-process telemetry is an error."""
    results = []
    load_windows = []
    for label, mode in workloads(getattr(args, "processes", 2)):
        path = out / f"{variant}-{label}.json"
        report = json.loads(path.read_text())
        load_windows.append(report.get("load_windows", []))
        validate(report, args.streams, getattr(args, "retention_seconds", 120), getattr(args, "clients", 64))
        if hasattr(args, "socket_buffer"):
            assert report["socket_buffer_bytes"] == args.socket_buffer
        if hasattr(args, "backend_host"):
            assert report["backend_host"] == args.backend_host
        if hasattr(args, "server_host"):
            assert report["server_host"] == args.server_host
        if hasattr(args, "cohort_settle"):
            assert duration_ns(report["cohort_settle"]) == duration_ns(args.cohort_settle)
        if hasattr(args, "synchronize_load"):
            assert report.get("load_synchronized", False) == args.synchronize_load
        if hasattr(args, "churn_interval"):
            assert duration_ns(report["churn_interval"]) == duration_ns(args.churn_interval)
        for key in ("reconnect_min", "reconnect_max"):
            if hasattr(args, key):
                assert duration_ns(report[key]) == duration_ns(getattr(args, key))
        results.append({"case": label, "mode": report["unconfirmed_mode"],
                        "verified_upload_bytes": report["verified_upload_bytes"],
                        "verified_download_bytes": report["verified_download_bytes"],
                        "release_milliseconds": report["unconfirmed_release_milliseconds"],
                        "application_release_milliseconds": report.get("unconfirmed_application_release_milliseconds"),
                        "unconfirmed_upgrade_retries": report.get("unconfirmed_upgrade_retries"),
                        "unconfirmed_probe_write_timeout": report.get("unconfirmed_probe_write_timeout"),
                        "closed": report["samples"][-1]})
    for pid in labels:
        measured = [s["details"].get("processes", {}).get(pid) for s in observations]
        measured = [s for s in measured if s is not None]
        tcp_counts = [s["tcp_descriptors"] for s in measured if s["tcp_descriptors"] is not None]
        if not tcp_counts or max(tcp_counts) <= getattr(args, "clients", 64):
            raise RuntimeError(f"missing TCP telemetry for test PID {pid}")
        if not any(s["rss_bytes"] for s in measured):
            raise RuntimeError(f"missing RSS telemetry for test PID {pid}")
    first, last = observations[0], observations[-1]
    row = {
        "variant": variant, "parallel_processes": getattr(args, "processes", 2), "streams_per_client": args.streams,
        "process_labels": labels, "start_offset_seconds": args.start_offset,
        "network_memory_denied_delta": last["network_memory_denied"] - first["network_memory_denied"],
        "pool_failure_deltas": {k: last["pool_failures"][k] - v for k, v in first["pool_failures"].items()},
        "event_count": len(events), "events": events, "results": results,
        "before_system": first, "after_system": last,
        "peak_tcp_descriptors": max(sum(v["tcp_descriptors"] or 0 for v in s["details"].get("processes", {}).values())
                                    for s in observations),
        "sampled_peak_combined_rss_bytes": max(sum(v["rss_bytes"] or 0 for v in s["details"].get("processes", {}).values())
                                               for s in observations),
        "cluster_pool_peaks": {size: {key: max(s["cluster_pools_kb"][size][key] for s in observations)
                                      for key in ("in_use", "allocated")}
                               for size in first["cluster_pools_kb"]},
        "tcp_inventory_missing_observations": sum(any(v["tcp_descriptors"] is None for v in s["details"].get("processes", {}).values()) for s in observations),
    }
    if all(load_windows):
        overlap = {}
        for round_number in {w["round"] for w in load_windows[0]}:
            windows = [next(w for w in group if w["round"] == round_number) for group in load_windows]
            assert all(w["streams"] == getattr(args, "clients", 64)*args.streams for w in windows)
            seconds = (min(w["send_until_unix_nano"] for w in windows) - max(w["ready_unix_nano"] for w in windows))/1e9
            if args.start_offset == 0:
                assert seconds > 0, f"round {round_number} did not overlap all processes at full application load"
            overlap[str(round_number)] = max(seconds, 0)
        row["full_load_overlap_seconds"] = overlap
    print(f"Passed parallel {variant}; denied delta={row['network_memory_denied_delta']}; "
          f"events={len(events)}", flush=True)
    return row


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path)
    parser.add_argument("--processes", type=int, choices=(2, 4, 8, 16), default=2)
    parser.add_argument("--clients", type=int, default=64)
    parser.add_argument("--backend-host", choices=("127.0.0.1", "::1"), default="127.0.0.1")
    parser.add_argument("--server-host", choices=("127.0.0.1", "::1"), default="127.0.0.1")
    parser.add_argument("--tunnel-log", action="store_true")
    parser.add_argument("--cohort-settle", default="0s")
    parser.add_argument("--synchronize-load", action="store_true")
    parser.add_argument("--streams", type=int, default=4)
    parser.add_argument("--socket-buffer", type=int, default=16384)
    parser.add_argument("--churn-interval", default="1m")
    parser.add_argument("--reconnect-min", default="5ms")
    parser.add_argument("--reconnect-max", default="25ms")
    parser.add_argument("--retention-seconds", type=int, default=120)
    parser.add_argument("--trace", action="store_true")
    parser.add_argument("--variants", nargs="+", choices=("plain", "race"), default=["plain", "race"])
    parser.add_argument("--start-offset", type=float, default=2)
    parser.add_argument("--interval", type=float, default=0.1)
    parser.add_argument("--before", type=float, default=10)
    parser.add_argument("--after", type=float, default=35)
    args = parser.parse_args()
    try:
        duration_ns(args.cohort_settle)
    except ValueError:
        parser.error("invalid cohort settle duration")
    if sys.platform != "darwin":
        parser.error("macOS netstat/lsof telemetry is required")
    if args.socket_buffer < 0 or not 6 <= args.retention_seconds <= 120 or args.clients < 1 or args.streams < 1 or args.interval <= 0 or args.before < 0 or args.after < 0 or args.start_offset < 0:
        parser.error("invalid streams or timing values")
    default_out = "network-memory-parallel" if args.processes == 2 else f"network-memory-parallel-{args.processes}"
    out = (args.output or ROOT / "artifacts" / default_out).resolve()
    out.mkdir(parents=True, exist_ok=True)
    summary = {"parallel_processes": args.processes, "clients_per_process": args.clients, "streams_per_client": args.streams,
               "backend_host": args.backend_host,
               "server_host": args.server_host,
               "cohort_settle": args.cohort_settle,
               "load_synchronized": args.synchronize_load,
               "socket_buffer_bytes": args.socket_buffer, "churn_interval": args.churn_interval,
               "reconnect_min": args.reconnect_min, "reconnect_max": args.reconnect_max, "session_retention": f"{args.retention_seconds}s", "sample_interval_seconds": args.interval,
               "process_detail_interval_seconds": 1, "variants_run_serially": True,
               "runs": []}
    summary_path = out / "summary.json"
    if summary_path.exists():
        existing = json.loads(summary_path.read_text())
        for key in ("parallel_processes", "clients_per_process", "streams_per_client", "backend_host", "server_host", "cohort_settle", "load_synchronized", "socket_buffer_bytes", "churn_interval", "reconnect_min", "reconnect_max", "session_retention"):
            if key in existing and existing[key] != summary[key]:
                parser.error(f"output directory has different {key}; use a separate output directory")
        summary["runs"] = existing["runs"]
    failure_path = out / "failures.json"
    failures = {row["variant"]: row for row in json.loads(failure_path.read_text())} if failure_path.exists() else {}
    for variant in args.variants:
        try:
            row = run_group(args, variant, out)
        except (RuntimeError, AssertionError) as exc:
            failures[variant] = {"variant": variant, "error": str(exc)}
            summary["runs"] = [r for r in summary["runs"] if r["variant"] != variant]
            (out / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
            print(f"FAILED {variant}: {exc}", flush=True)
            continue
        failures.pop(variant, None)
        summary["runs"] = [r for r in summary["runs"] if r["variant"] != variant] + [row]
        (out / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
    if failures:
        failure_path.write_text(json.dumps(list(failures.values()), indent=2) + "\n")
        raise SystemExit(1)
    failure_path.unlink(missing_ok=True)


if __name__ == "__main__":
    main()
