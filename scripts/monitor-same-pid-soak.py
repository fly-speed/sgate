#!/usr/bin/env python3
"""Capture native memory/socket state when post-GC same-PID RSS keeps growing."""

import argparse
import json
import pathlib
import subprocess
import time


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=pathlib.Path, required=True)
    parser.add_argument(
        "--capture-final",
        action="store_true",
        help="capture native state at the last idle cycle, before shutdown",
    )
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=True)
    baselines, seen, captured = {}, set(), set()
    deadline = time.monotonic() + 2400
    while time.monotonic() < deadline:
        results = list(args.output.glob("*-ws*/W*/result.json"))
        if len(results) == 4:
            break
        for case in pathlib.Path("/private/tmp").glob("sgate-acl-*"):
            config = case / "conf/service/server.cf"
            if not config.exists() or str(args.output.resolve()) not in config.read_text():
                continue
            for role in ["server", "client"]:
                path = config.with_name(role + ".cf.metrics")
                try:
                    value = json.loads(path.read_text().splitlines()[-1])
                except (OSError, ValueError, IndexError):
                    continue
                phase = value["phase"]
                final_phase = (
                    phase == "same-pid-idle-9"
                    or phase.startswith("observed-idle-9-")
                    or phase == "final-idle"
                )
                if args.capture_final and not final_phase:
                    continue
                if not args.capture_final and not phase.startswith("same-pid-idle-"):
                    continue
                key = (value["pid"], phase)
                if key in seen:
                    continue
                seen.add(key)
                # Start after the first full cycle; retain every measurement,
                # including the first, in the service's own metrics file.
                if (not args.capture_final and phase.endswith("-0")) or value["rss_bytes"] < 0:
                    continue
                baseline = baselines.setdefault(value["pid"], value)
                trigger = (
                    args.capture_final
                    or value["rss_bytes"] > baseline["rss_bytes"] + 64 * 1024 * 1024
                )
                record = dict(
                    role=role,
                    phase=phase,
                    pid=value["pid"],
                    rss_bytes=value["rss_bytes"],
                    heap_bytes=value["heap_bytes"],
                    baseline_rss_bytes=baseline["rss_bytes"],
                    trigger=trigger,
                )
                prefix = "native-final" if args.capture_final else "native-memory"
                with (args.output / (prefix + "-monitor.jsonl")).open("a") as stream:
                    stream.write(json.dumps(record) + "\n")
                if not trigger or value["pid"] in captured:
                    continue
                captured.add(value["pid"])
                name = args.output / (
                    ("native-final-" if args.capture_final else "native-memory-growth-")
                    + str(value["pid"])
                )
                config.with_name(role + ".cf.snapshot").write_text(
                    (
                        "end-of-soak native capture: "
                        if args.capture_final
                        else "post-warmup RSS growth: "
                    )
                    + json.dumps(record)
                )
                name.with_suffix(".json").write_text(
                    json.dumps(dict(baseline=baseline, current=value), indent=2) + "\n"
                )
                for label, command in [
                    ("ps", ["ps", "-o", "pid,ppid,rss,vsz,etime,command", "-p", str(value["pid"])]),
                    ("sockets", ["lsof", "-nP", "-a", "-p", str(value["pid"]), "-i"]),
                    ("vmmap", ["vmmap", "-summary", str(value["pid"])]),
                ]:
                    try:
                        result = subprocess.run(command, capture_output=True, text=True, timeout=15)
                        text = (
                            "exit=" + str(result.returncode) + "\n" + result.stdout + result.stderr
                        )
                    except (OSError, subprocess.TimeoutExpired) as error:
                        text = str(error)
                    (args.output / (name.name + "-" + label + ".txt")).write_text(text)
        time.sleep(0.2)
    (
        args.output
        / (("native-final" if args.capture_final else "native-memory") + "-monitor-result.json")
    ).write_text(
        json.dumps(
            dict(monitored_pids=sorted(baselines), rss_growth_captures=sorted(captured)), indent=2
        )
        + "\n"
    )


if __name__ == "__main__":
    main()
