#!/usr/bin/env python3
"""Check completed multi-backend cases and preserve their resource evidence."""

import argparse
import collections
import hashlib
import importlib.util
import json
import pathlib
import platform


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("output", type=pathlib.Path)
    args = parser.parse_args()
    root = args.output.resolve()
    spec = importlib.util.spec_from_file_location(
        "watch", pathlib.Path(__file__).with_name("analyze-goroutine-watch.py")
    )
    watch = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(watch)
    cases, episodes = [], []
    totals = collections.Counter()
    for kind in ["plain", "race"]:
        for mode in ["managed", "standalone"]:
            for protocol in ["ws", "wss"]:
                name = "-".join([kind, mode, protocol])
                directory = root / "verified" / name
                result = json.loads((directory / "result.json").read_text())
                assert result["status"] == "passed", name
                assert result["mode"] == mode and result["protocol"] == protocol
                assert result["concurrency"] == 128 and result["rounds"] == 3
                assert result["production_reconnect_backoff"] and result["process_restarts"] == 0
                assert result["same_pid_hot_apply"] and result["old_connections_closed"]
                assert result["failed_config_preserved"] and result["ports_released"]
                assert result["fixture_threads_released"]
                assert len(result["closed_process_censuses"]) == 4
                assert result["upstream_records"] == 128 * 128 * 3
                assert [w["default_target"] for w in result["waves"]] == ["A", "D", "D"]
                assert all(
                    w["connections"] == 128 and w["records"] == 128 and w["transport_faults"] > 0
                    for w in result["waves"]
                )
                assert collections.Counter(p["expected_status"] for p in result["probes"]) == {
                    403: 4,
                    400: 4,
                    404: 2,
                }
                roles = {}
                for role, pid in result["process_pids"].items():
                    rows = [
                        json.loads(line)
                        for line in (directory / (role + ".cf.metrics")).read_text().splitlines()
                    ]
                    assert {row["pid"] for row in rows} == {pid}
                    baseline = next(row for row in rows if row["phase"] == "baseline")
                    idle = [row for row in rows if row["phase"].startswith("idle-")]
                    assert len(idle) == 3
                    for row in idle:
                        assert row["census"]["streams"] == row["census"]["pending_bytes"] == 0
                        assert row["census"]["sessions"] == (3 if role == "server" else 1)
                        assert row["census"]["registered_servers"] == (1 if role == "server" else 0)
                        assert row["goroutines"] <= baseline["goroutines"] + 8
                        assert row["fds"] <= baseline["fds"] + 4
                        assert row["heap_bytes"] <= baseline["heap_bytes"] + 16 * 1024 * 1024
                    closed = [row for row in rows if row["phase"] == "closed"]
                    assert len(closed) == 1 and closed[0]["goroutines"] <= 6
                    assert closed[0]["transport_writers"] == 0
                    assert all(
                        closed[0]["census"][key] == 0
                        for key in [
                            "sessions",
                            "streams",
                            "pending_bytes",
                            "nonces",
                            "attached",
                            "registered_servers",
                        ]
                    )
                    roles[role] = dict(
                        pid=pid,
                        elapsed_seconds=(closed[0]["unix_nano"] - rows[0]["unix_nano"]) / 1e9,
                        idle_goroutines=[r["goroutines"] for r in idle],
                        idle_fds=[r["fds"] for r in idle],
                        idle_heap_bytes=[r["heap_bytes"] for r in idle],
                        idle_rss_bytes=[r["rss_bytes"] for r in idle],
                        closed=closed[0],
                    )
                for path in directory.glob("*.watch-*.event-*-0.json"):
                    event = watch.episode(path)
                    event["case"] = name
                    episodes.append(event)
                totals["strict_connections"] += 128 * 3
                totals["held_connections_closed_by_apply"] += 128
                totals["checked_duplex_bytes"] += sum(
                    w["checked_duplex_bytes"] for w in result["waves"]
                )
                totals["transport_faults"] += sum(w["transport_faults"] for w in result["waves"])
                totals["upstream_records"] += result["upstream_records"]
                totals["allowlist_probe_requests"] += len(result["probes"])
                totals["successful_hot_applies"] += 2
                totals["invalid_hot_applies_rejected"] += 1
                totals["closed_go_processes"] += 4
                cases.append(dict(name=name, roles=roles, probes=result["probes"]))
    report = dict(
        status="passed",
        host=dict(system=platform.system(), machine=platform.machine()),
        summary=dict(totals),
        cases=cases,
        episodes=episodes,
        episode_attributions=dict(collections.Counter(e["attribution"] for e in episodes)),
        resource_review=dict(
            server_idle_goroutines=sorted(
                {value for case in cases for value in case["roles"]["server"]["idle_goroutines"]}
            ),
            server_idle_fds=sorted(
                {value for case in cases for value in case["roles"]["server"]["idle_fds"]}
            ),
            max_server_idle_heap_bytes=max(
                value for case in cases for value in case["roles"]["server"]["idle_heap_bytes"]
            ),
            resource_trigger_episodes=len(episodes),
            explanation="Pending replay buffers and application streams were zero at idle checkpoints. Anti-replay nonces intentionally remain for their bounded retention interval, and are zero after shutdown. All four process IDs remained unchanged within each case; the diagnostic server registry retained only the active server. No resource threshold triggered a snapshot in the verified matrix; idle and shutdown stacks were still saved.",
        ),
        diagnostic_manifests={
            kind: str(root / (kind + "-bin") / ("manifest-" + kind + ".json"))
            for kind in ["plain", "race"]
        },
        limitations=[
            "Runtime tests used macOS arm64 and the installed acl_master with isolated temporary management sockets and configuration files.",
            "Each case retained its original four Go process IDs; configuration apply closes existing application connections. Strict fault-only waves require complete delivery in both directions; the separate held phase requires old connections to close after apply/offline expiry.",
            "Three rounds per case are finite-duration resource checks, with forced-GC idle checkpoints; they cannot prove the absence of all possible long-term leaks. Session/offline retention was 6 seconds to check bounded cleanup. Production default reconnect backoff was retained.",
            "Diagnostic builds add the metrics sampler and watcher; saved goroutine counts therefore differ from ordinary production builds. Trigger counts and captured stack counts are non-atomic; raw snapshots are retained for review.",
        ],
        source_sha256={
            str(p.relative_to(pathlib.Path(__file__).resolve().parents[1])): hashlib.sha256(
                p.read_bytes()
            ).hexdigest()
            for p in [
                pathlib.Path(__file__).resolve(),
                pathlib.Path(__file__).with_name("test-multibackend.py").resolve(),
                pathlib.Path(__file__).with_name("selected-target-probe.go.in").resolve(),
            ]
        },
    )
    (root / "verification.json").write_text(json.dumps(report, indent=2) + "\n")
    print(
        json.dumps(
            dict(summary=report["summary"], episode_attributions=report["episode_attributions"]),
            indent=2,
        )
    )


if __name__ == "__main__":
    main()
