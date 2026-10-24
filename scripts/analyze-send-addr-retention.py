#!/usr/bin/env python3
"""Validate real default-retention results and timestamped resource release."""

import argparse
import importlib.util
import json
import pathlib

spec = importlib.util.spec_from_file_location(
    "watch", pathlib.Path(__file__).with_name("analyze-goroutine-watch.py")
)
watch = importlib.util.module_from_spec(spec)
spec.loader.exec_module(watch)


def analyze_case(folder):
    result = json.loads((folder / "result.json").read_text())
    assert result["status"] == "passed" and result["retention_seconds"] == 120
    assert result["concurrency"] == 128 and result["ports_released"]
    assert result["fixture_threads_released"]
    events = result["events"]
    retained = next(e for e in events if e["event"] == "retained-data-verified")
    assert 75 <= retained["outage_seconds"] < 120 and retained["connections"] == 128
    cut = next(e for e in events if e["event"] == "beyond-window")["disconnected_unix_nano"]
    applied = [e for e in events if e["event"] == "offline-apply"]
    assert len(applied) == 4
    assert [e["fields"]["send-addr"] for e in applied] == ["false", "true", "false", "true"]
    restart = next(e for e in events if e["event"] == "server-restarted")
    assert restart["old_pid"] != restart["new_pid"] == result["process_pids"]["server"]
    assert len(result["waves"]) == 2 and result["upstream_records"] == 128 * 128 * 3
    assert [w["default_target"] for w in result["waves"]] == ["A", "D"]
    resources = {}
    server_rows = None
    for role in ["server", "client0", "client1", "client2"]:
        rows = [
            json.loads(line) for line in (folder / (role + ".cf.metrics")).read_text().splitlines()
        ]
        phases = {row["phase"]: row for row in rows if row["phase"] != "load"}
        baseline = phases["baseline"]
        if role == "server":
            server_rows = rows
            assert baseline["census"]["session_ttl_ns"] == 120_000_000_000
        else:
            assert baseline["endpoints"][0]["offline_timeout_ns"] == 120_000_000_000
            assert {row["pid"] for row in rows} == {baseline["pid"]}
        for phase in ["hold-80", "hold-81", "hold-83"]:
            assert phases[phase]["census"]["streams"] == (
                128 if role == "server" else 42 if role == "client2" else 43
            )
        for phase in ["queued-80", "queued-81", "queued-83"]:
            assert phases[phase]["census"]["pending_bytes"] > 0
        before = phases["before-resume"]
        assert before["census"]["pending_bytes"] > 0
        if role == "server":
            assert before["census"]["streams"] == 128 and before["census"]["sessions"] == 3
        expired = phases["expired-idle"]
        assert expired["census"]["streams"] == expired["census"]["pending_bytes"] == 0
        assert expired["transport_writers"] == 0
        if role == "server":
            assert expired["census"]["sessions"] == expired["census"]["attached"] == 0
        idle = [row for row in rows if row["phase"] == "baseline" or row["phase"].endswith("-idle")]
        assert len(idle) == 6
        for row in idle:
            assert row["census"]["streams"] == row["census"]["pending_bytes"] == 0
            assert 0 < row["fds"] <= baseline["fds"] + 4
            assert row["goroutines"] <= baseline["goroutines"] + 8
            assert row["heap_bytes"] <= baseline["heap_bytes"] + 16 * 1024 * 1024
        closed = [row for row in rows if row["phase"] == "closed"]
        assert len(closed) == (2 if role == "server" else 1)
        for row in closed:
            assert row["goroutines"] <= 6 and row["transport_writers"] == 0
            assert row["endpoints"] == []
            assert all(
                row["census"][key] == 0
                for key in [
                    "sessions",
                    "streams",
                    "pending_bytes",
                    "nonces",
                    "attached",
                    "registered_servers",
                ]
            )
            stack = (folder / f"{role}.cf.metrics.closed-{row['pid']}.stack").read_text()
            assert not any(
                frame in stack
                for frame in [
                    "sgate/internal/tunnel.",
                    "sgate/internal/admin.",
                    "sgate/internal/app.",
                ]
            )
        if role != "server":
            initial = baseline["endpoints"][0]
            final = phases["final-idle"]["endpoints"][0]
            assert final["addr"] == (initial["addr"] if role == "client1" else None)
            assert final["omit_addr"] == (role != "client1")
            assert final["keep"] == "unchanged"
        resources[role] = dict(
            idle_goroutines=[row["goroutines"] for row in idle],
            idle_heap_bytes=[row["heap_bytes"] for row in idle],
            idle_rss_bytes=[row["rss_bytes"] for row in idle],
            idle_fds=[row["fds"] for row in idle],
            closed_pids=[row["pid"] for row in closed],
        )
    near = [row for row in server_rows if 115 <= (row["unix_nano"] - cut) / 1e9 < 119]
    assert near and all(
        row["census"]["streams"] == 128 and row["census"]["sessions"] == 3 for row in near
    )
    released = next(
        row
        for row in server_rows
        if row["unix_nano"] > cut
        and row["pid"] == restart["old_pid"]
        and row["census"]["sessions"] == 0
        and row["census"]["streams"] == row["census"]["pending_bytes"] == 0
    )
    release_seconds = (released["unix_nano"] - cut) / 1e9
    assert 119 <= release_seconds <= 125, release_seconds
    episodes = [watch.episode(p) for p in sorted(folder.glob("*.watch-*.event-*-0.json"))]
    return dict(
        case=folder.name,
        resources=resources,
        release_seconds=release_seconds,
        retained_outage_seconds=retained["outage_seconds"],
        episodes=episodes,
        checked_duplex_bytes=retained["checked_duplex_bytes"]
        + sum(w["checked_duplex_bytes"] for w in result["waves"]),
    )


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("output", type=pathlib.Path)
    args = parser.parse_args()
    cases = [
        analyze_case(args.output / "cases" / f"{mode}-{tls}")
        for mode in ["standalone", "managed"]
        for tls in ["ws", "wss"]
    ]
    result = dict(
        status="passed",
        cases=cases,
        strict_connections=1536,
        expired_or_replaced_connections=1024,
        server_process_restarts=4,
        checked_duplex_bytes=sum(case["checked_duplex_bytes"] for case in cases),
        captured_episodes=sum(len(case["episodes"]) for case in cases),
    )
    (args.output / "verification.json").write_text(json.dumps(result, indent=2) + "\n")
    print(json.dumps({key: value for key, value in result.items() if key != "cases"}, indent=2))


if __name__ == "__main__":
    main()
