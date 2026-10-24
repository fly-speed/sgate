#!/usr/bin/env python3
"""Opt-in macOS TCP port control: no sgate, payload, or host setting changes."""

import argparse
import datetime
import json
from pathlib import Path
import resource
import socket
import subprocess
import sys


def command(*args):
    result = subprocess.run(args, capture_output=True, text=True, timeout=10)
    return dict(exit=result.returncode, stdout=result.stdout, stderr=result.stderr)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--max-connections", type=int, default=18000)
    args = parser.parse_args()
    if sys.platform != "darwin" or not 1 <= args.max_connections <= 20000:
        parser.error("requires macOS and 1..20000 connections")
    args.output.parent.mkdir(parents=True, exist_ok=True)
    soft, hard = resource.getrlimit(resource.RLIMIT_NOFILE)
    ceiling = 70000 if hard == resource.RLIM_INFINITY else hard
    # Raise this process's soft descriptor budget only, never a host limit.
    resource.setrlimit(resource.RLIMIT_NOFILE, (max(soft, min(70000, ceiling)), hard))
    result = dict(
        started=datetime.datetime.now().astimezone().isoformat(),
        fd_limits_before=[soft, hard],
        host=command(
            "sysctl",
            "hw.memsize",
            "net.inet.ip.portrange.first",
            "net.inet.ip.portrange.last",
            "kern.ipc.somaxconn",
        ),
        before_mbuf=command("netstat", "-mmm"),
    )
    held, listeners = [], []

    def listener(family, host):
        sock = socket.socket(family, socket.SOCK_STREAM)
        listeners.append(sock)
        sock.bind((host, 0))
        sock.listen(128)
        sock.settimeout(2)
        return sock

    def pair(server):
        client = socket.socket(server.family, socket.SOCK_STREAM)
        client.settimeout(0.5)
        try:
            client.connect(server.getsockname())
            peer, _ = server.accept()
        except OSError:
            client.close()
            raise
        held.extend((client, peer))
        return client

    try:
        targets = [listener(socket.AF_INET, "127.0.0.1") for _ in range(16)]
        ports, count = set(), 0
        for i in range(args.max_connections):
            try:
                client = pair(targets[i % len(targets)])
            except OSError as exc:
                result["ipv4_error"] = dict(
                    errno=exc.errno,
                    message=str(exc),
                    connections=count,
                    unique_source_ports=len(ports),
                )
                break
            ports.add(client.getsockname()[1])
            count += 1
        result["ipv4_connections"] = count
        for label, server in (
            ("another_ipv4_target", listener(socket.AF_INET, "127.0.0.1")),
            ("ipv6_target", listener(socket.AF_INET6, "::1")),
        ):
            successes, newports = 0, set()
            for _ in range(256):
                try:
                    client = pair(server)
                except OSError as exc:
                    result[label] = dict(successes=successes, errno=exc.errno, message=str(exc))
                    break
                successes += 1
                newports.add(client.getsockname()[1])
            else:
                result[label] = dict(
                    successes=successes, ports_also_held_by_ipv4=len(newports & ports)
                )
        result.update(
            peak_mbuf=command("netstat", "-mmm"),
            peak_tcp=command("netstat", "-an", "-p", "tcp"),
            peak_vm=command("vm_stat"),
            peak_rss_bytes=resource.getrusage(resource.RUSAGE_SELF).ru_maxrss,
        )
    finally:
        for sock in held + listeners:
            sock.close()
    result.update(
        after_close_mbuf=command("netstat", "-mmm"),
        closed_at=datetime.datetime.now().astimezone().isoformat(),
    )
    args.output.write_text(json.dumps(result, indent=2) + "\n")
    print(
        json.dumps(
            {
                k: result[k]
                for k in ("ipv4_connections", "ipv4_error", "another_ipv4_target", "ipv6_target")
                if k in result
            },
            indent=2,
        )
    )


if __name__ == "__main__":
    main()
