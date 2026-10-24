#!/usr/bin/env python3
"""Validate the complete client address switching matrix and summarize resources."""

import argparse
import importlib.util
import json
import pathlib

spec = importlib.util.spec_from_file_location(
    "watch", pathlib.Path(__file__).with_name("analyze-goroutine-watch.py")
)
watch = importlib.util.module_from_spec(spec)
spec.loader.exec_module(watch)


def validate_toggle_endpoints(rows, role):
    phases = [
        "baseline",
        "disable-idle",
        "restore-url-idle",
        "disable-url-idle",
        "default-D-idle",
        "restore-independent-idle",
        "disable-independent-idle",
        "default-A-idle",
        "restore-url-again-idle",
        "final-idle",
    ]
    samples = {row["phase"]: row for row in rows}
    endpoints = [samples[phase]["endpoints"] for phase in phases]
    assert all(len(values) == 1 for values in endpoints)
    endpoints = [values[0] for values in endpoints]
    if role == "client0":
        b = endpoints[0]["addr"][0]
        c = endpoints[2]["addr"][0]
        assert b != c
        selections = [b, None, c, None, None, b, None, None, c, c]
        configured = [b, b, "", "", "", b, b, b, "", ""]
    elif role == "client1":
        b = endpoints[0]["addr"][0]
        selections, configured = [b] * 10, [b] * 10
    else:
        selections, configured = [None] * 10, [""] * 10
    for endpoint, selected, saved in zip(endpoints, selections, configured):
        assert endpoint["addr"] == ([selected] if selected else None), endpoint
        assert endpoint["omit_addr"] == (selected is None), endpoint
        assert endpoint["configured_addr"] == saved, endpoint
        assert endpoint["keep"] == "unchanged" and endpoint["path"] == "/tunnel"
    return endpoints


def analyze(root, send_addr_toggle=False):
    wave_count, transition_count, idle_count = (9, 8, 10) if send_addr_toggle else (5, 4, 7)
    cases = []
    for build in ["plain", "race"]:
        for mode in ["standalone", "managed"]:
            for protocol in ["ws", "wss"]:
                label = f"{build}-{mode}-{protocol}"
                folder = root / "cases" / label
                result = json.loads((folder / "result.json").read_text())
                assert result["status"] == "passed", label
                assert result["concurrency"] == 128 and len(result["waves"]) == wave_count
                if send_addr_toggle:
                    assert result["scenario"] == "send-addr"
                    assert [w["default_target"] for w in result["waves"]] == [
                        "A",
                        "A",
                        "A",
                        "A",
                        "D",
                        "D",
                        "D",
                        "A",
                        "A",
                    ]
                assert result["process_restarts"] == 0
                assert result["ports_released"] and result["fixture_threads_released"]
                assert result["upstream_records"] == 128 * 128 * wave_count
                assert len(result["transitions"]) == transition_count
                assert all(
                    w["transport_faults"] > 0 and w["connections"] == 128 for w in result["waves"]
                )
                resources = {}
                for role in ["server", "client0", "client1", "client2"]:
                    rows = [
                        json.loads(line)
                        for line in (folder / f"{role}.cf.metrics").read_text().splitlines()
                    ]
                    baseline = next(r for r in rows if r["phase"] == "baseline")
                    endpoints = None
                    if send_addr_toggle and role != "server":
                        endpoints = validate_toggle_endpoints(rows, role)
                    idle = [
                        r for r in rows if r["phase"] == "baseline" or r["phase"].endswith("-idle")
                    ]
                    assert len(idle) == idle_count, (label, role, [r["phase"] for r in idle])
                    for row in idle:
                        assert row["pid"] == baseline["pid"]
                        assert row["fds"] > 0 and row["rss_bytes"] > 0
                        assert row["goroutines"] <= baseline["goroutines"] + 8
                        assert row["fds"] <= baseline["fds"] + 4
                        assert row["heap_bytes"] <= baseline["heap_bytes"] + 16 * 1024 * 1024
                        assert row["census"]["streams"] == row["census"]["pending_bytes"] == 0
                        if send_addr_toggle:
                            assert row["census"]["sessions"] == (3 if role == "server" else 1)
                            assert row["census"]["attached"] == (3 if role == "server" else 0)
                    closed = [r for r in rows if r["phase"] == "closed"]
                    assert len(closed) == 1 and closed[0]["goroutines"] <= 6
                    assert closed[0]["transport_writers"] == 0
                    if send_addr_toggle:
                        assert closed[0]["endpoints"] == []
                    stacks = list(folder.glob(f"{role}.cf.metrics.closed-*.stack"))
                    assert len(stacks) == 1
                    stack = stacks[0].read_text()
                    assert all(
                        frame not in stack
                        for frame in [
                            "sgate/internal/tunnel.",
                            "sgate/internal/admin.",
                            "sgate/internal/app.",
                        ]
                    ), stacks[0]
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
                    timeline = []
                    for path in folder.glob(f"{role}.cf.metrics.watch-*.jsonl"):
                        timeline.extend(json.loads(line) for line in path.read_text().splitlines())
                    # Shutdown may run between the final sample and watcher stop.
                    # Compare the live idle state, then check closed metrics separately.
                    end = next(row["unix_nano"] for row in rows if row["phase"] == "final-idle")
                    timeline = [row for row in timeline if row["unix_nano"] <= end]
                    assert timeline and timeline[-1]["tracked_connections"] == (
                        3 if role == "server" else 1
                    )
                    assert (
                        timeline[-1]["census"]["streams"]
                        == timeline[-1]["census"]["pending_bytes"]
                        == 0
                    )
                    resources[role] = {
                        "idle_endpoints": endpoints,
                        "timeline_samples": len(timeline),
                        "final_tracked_connections": timeline[-1]["tracked_connections"],
                        "idle_goroutines": [r["goroutines"] for r in idle],
                        "idle_fds": [r["fds"] for r in idle],
                        "idle_heap_bytes": [r["heap_bytes"] for r in idle],
                        "idle_rss_bytes": [r["rss_bytes"] for r in idle],
                        "closed_goroutines": closed[0]["goroutines"],
                    }
                episodes = [
                    watch.episode(path) for path in sorted(folder.glob("*.watch-*.event-*-0.json"))
                ]
                cases.append(
                    dict(
                        case=label,
                        resources=resources,
                        episodes=episodes,
                        checked_duplex_bytes=sum(
                            w["checked_duplex_bytes"] for w in result["waves"]
                        ),
                        transport_faults=sum(w["transport_faults"] for w in result["waves"]),
                    )
                )
    return dict(
        status="passed",
        cases=cases,
        strict_connections=8 * wave_count * 128,
        held_connections=8 * transition_count * 128,
        checked_duplex_bytes=sum(c["checked_duplex_bytes"] for c in cases),
        transport_faults=sum(c["transport_faults"] for c in cases),
        captured_episodes=sum(len(c["episodes"]) for c in cases),
    )


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("output", type=pathlib.Path)
    parser.add_argument("--send-addr-toggle", action="store_true")
    args = parser.parse_args()
    result = analyze(args.output, args.send_addr_toggle)
    (args.output / "verification.json").write_text(json.dumps(result, indent=2) + "\n")
    print(json.dumps({key: value for key, value in result.items() if key != "cases"}, indent=2))
