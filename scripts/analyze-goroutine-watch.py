#!/usr/bin/env python3
"""Summarize live goroutine ownership and recovery from diagnostic snapshots."""

import argparse
import collections
import json
import pathlib
import re


def stack_summary(path):
    blocks = path.read_text().strip().split("\n\n")
    groups = collections.Counter()
    stream_readers = mutex_readers = 0
    for block in blocks:
        lines = block.splitlines()
        if not lines or not lines[0].startswith("goroutine "):
            raise ValueError(f"invalid goroutine block in {path}")
        frame = next(line for line in lines[1:] if not line.startswith("\t"))
        groups[re.sub(r"\([^()]*\)$", "", frame)] += 1
        if "(*session).readStream(" in block:
            stream_readers += 1
            mutex_readers += "[sync.Mutex.Lock]" in lines[0]
    return dict(
        count=len(blocks),
        stream_readers=stream_readers,
        mutex_stream_readers=mutex_readers,
        top_frames=dict(groups.most_common()),
    )


def episode(path):
    samples = []
    for step in range(5):
        source = path.with_name(path.name.removesuffix("0.json") + str(step) + ".json")
        if not source.exists():
            continue  # Process replacement can end follow-up before ten seconds.
        sample = json.loads(source.read_text())
        sample["stack"] = stack_summary(source.with_suffix(".stack"))
        sample["source"] = str(source)
        sample["normalized_goroutines"] = sample["goroutines"] - sample["census"]["streams"]
        samples.append(sample)
    first = samples[0]
    if first["census"]["streams"]:
        attribution = "application traffic resumed before the snapshot"
    elif first["stack"]["mutex_stream_readers"]:
        attribution = "stream readers leaving a closed/replaced session"
    elif first["stack"]["stream_readers"]:
        attribution = "stream readers still exiting"
    elif first["stack"]["count"] <= 24:
        attribution = "elevated trigger already fell before the stack snapshot"
    else:
        attribution = "requires manual stack review"
    idle_recovery = [
        s["elapsed_ms"]
        for s in samples
        if s["goroutines"] <= 24
        and s["census"]["streams"] == 0
        and s["census"]["pending_bytes"] == 0
    ]
    normalized_recovery = [s["elapsed_ms"] for s in samples if s["normalized_goroutines"] <= 24]
    return dict(
        role="server" if path.name.startswith("server.") else "client",
        pid=first["pid"],
        trigger_goroutines=first["trigger_goroutines"],
        attribution=attribution,
        first_idle_recovery_ms=min(idle_recovery, default=None),
        first_normalized_recovery_ms=min(normalized_recovery, default=None),
        followup_ended_early=len(samples) < 5,
        samples=samples,
    )


def analyze(output):
    result = json.loads((output / "result.json").read_text())
    assert result["status"] == "passed", output
    stress = result["stress"]
    assert stress["concurrency"] == 128 and stress["rounds"] == 8, stress
    assert stress["mixed_seconds"] == 30 and stress["idle_seconds"] == 10, stress
    episodes = [episode(path) for path in sorted(output.glob("*.watch-*.event-*-0.json"))]
    resources = {}
    for role in ["server", "client"]:
        rows = [
            json.loads(line) for line in (output / f"{role}.cf.metrics").read_text().splitlines()
        ]
        baseline = next(r for r in rows if r["phase"] == "baseline")
        final = next(r for r in rows if r["phase"] == "final-idle")
        closed = [r for r in rows if r["phase"] == "closed"]
        idle = [r for r in rows if r["phase"].startswith("observed-idle-")]
        assert len(idle) >= 8 * 3, (output, role, len(idle))
        assert final["goroutines"] <= baseline["goroutines"] + 8
        assert final["fds"] <= baseline["fds"] + 4
        assert final["heap_bytes"] <= baseline["heap_bytes"] + 12 * 1024 * 1024
        assert all(
            r["transport_writers"] == 0
            and r["goroutines"] <= 6
            and all(
                r["census"][k] == 0
                for k in ["sessions", "streams", "pending_bytes", "nonces", "attached"]
            )
            for r in closed
        )
        timelines = []
        for path in output.glob(f"{role}.cf.metrics.watch-*.jsonl"):
            timelines.extend(json.loads(line) for line in path.read_text().splitlines())
        resources[role] = dict(
            baseline=baseline,
            final=final,
            idle_observations=len(idle),
            max_observed_idle_goroutines=max(r["goroutines"] for r in idle),
            closed_processes=len(closed),
            closed_max_goroutines=max(r["goroutines"] for r in closed),
            timeline_samples=len(timelines),
            first_unix_nano=min(r["unix_nano"] for r in timelines),
            last_unix_nano=max(r["unix_nano"] for r in timelines),
        )
    return dict(output=str(output), result=result, resources=resources, episodes=episodes)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("output", type=pathlib.Path)
    parser.add_argument("--report", type=pathlib.Path, required=True)
    args = parser.parse_args()
    report = analyze(args.output)
    args.report.write_text(json.dumps(report, indent=2) + "\n")
    print(
        json.dumps(
            {"episodes": len(report["episodes"]), "resources": report["resources"]}, indent=2
        )
    )
