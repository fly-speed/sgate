#!/usr/bin/env python3
"""Read-only macOS/Linux VPN HTTPS comparison. No cookies, credentials or network changes."""
import argparse
import concurrent.futures
import datetime
import ipaddress
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import time

HOSTS = ("mail.google.com", "accounts.google.com", "www.gstatic.com", "ssl.gstatic.com")
FIELDS = ("http_code", "http_version", "remote_ip", "time_namelookup", "time_connect",
          "time_appconnect", "time_starttransfer", "time_total", "size_download",
          "speed_download", "ssl_verify_result", "exitcode")


def command(args, timeout=8):
    try:
        p = subprocess.run(args, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, timeout=timeout)
        return {"exitcode": p.returncode, "stdout": p.stdout, "stderr": p.stderr[:1000]}
    except subprocess.TimeoutExpired:
        return {"exitcode": 124, "stdout": "", "stderr": "command timed out"}
    except OSError as error:
        return {"exitcode": 127, "stdout": "", "stderr": str(error)}


def route(address):
    args = ["route", "-n", "get", address] if sys.platform == "darwin" else ["ip", "-4", "route", "get", address]
    raw = command(args)
    result = {"exitcode": raw["exitcode"]}
    if raw["exitcode"]:
        result["error"] = raw["stderr"]
    elif sys.platform == "darwin":
        for line in raw["stdout"].splitlines():
            key, _, value = line.strip().partition(":")
            if key in ("destination", "gateway", "interface"):
                result[key] = value.strip()
    else:
        fields = raw["stdout"].split()
        for key, output in (("dev", "interface"), ("via", "gateway"), ("src", "source")):
            if key in fields and fields.index(key) + 1 < len(fields):
                result[output] = fields[fields.index(key) + 1]
    return result


def resolve(host):
    code = "import socket,json,sys;print(json.dumps(sorted({r[4][0] for r in socket.getaddrinfo(sys.argv[1],443,type=socket.SOCK_STREAM)})))"
    p = command([sys.executable, "-c", code, host])
    if p["exitcode"]:
        return {"error": p["stderr"]}
    return {"addresses": json.loads(p["stdout"])}


def dns_overrides():
    if sys.platform != "darwin":
        return []
    raw = command(["scutil", "--dns"])
    if raw["exitcode"]:
        return [{"error": raw["stderr"]}]
    result = []
    for block in re.split(r"(?=resolver #)", raw["stdout"]):
        domain = re.search(r"^\s*domain\s*:\s*(\S+)", block, re.M)
        if domain and any(host == domain[1] or host.endswith("." + domain[1]) for host in HOSTS):
            result.append({"domain": domain[1], "nameservers": re.findall(r"nameserver\[\d+\]\s*:\s*(\S+)", block)})
    return result


def probe(curl, host, protocol, address, timeout, download=False):
    args = [curl, "-q", "--proxy", "", "--noproxy", "*", "-4", "--" + protocol,
            "--proto", "=https", "--proto-redir", "=https", "--connect-timeout", "8",
            "--max-time", str(timeout), "--silent", "--show-error", "--output", os.devnull,
            "--write-out", "%{json}"]
    if address:
        args += ["--resolve", host + ":443:" + address]
    if download:
        # Sample public, unauthenticated HTML. Response bodies/headers are discarded.
        args += ["--location", "--max-redirs", "5", "--range", "0-65535", "--max-filesize", "2097152"]
    else:
        args += ["--head"]
    args += ["https://" + host + ("/mail/u/0/" if host == "mail.google.com" else "/")]
    p = command(args, timeout + 3)
    try:
        metadata = json.loads(p["stdout"])
        result = {key: metadata[key] for key in FIELDS if key in metadata}
    except (ValueError, TypeError):
        result = {"parse_error": "curl JSON unavailable; curl >= 7.70 required"}
    result.update(host=host, requested_protocol=protocol, pinned_ip=address,
                  kind="download_sample" if download else "https_head", command_exitcode=p["exitcode"])
    if p["exitcode"]:
        result["error"] = p["stderr"]
    # A public HEAD 3xx/4xx response proves HTTPS reachability, not Gmail login.
    result["https_response_received"] = int(result.get("http_code", 0) or 0) >= 100
    result["transfer_completed"] = p["exitcode"] == 0
    if p["exitcode"] == 63:
        result["stage"] = "sample_size_limit"
    elif p["exitcode"] == 0:
        result["stage"] = "complete"
    elif result.get("size_download", 0) > 0:
        result["stage"] = "partial_body"
    elif p["exitcode"] == 6:
        result["stage"] = "dns"
    elif not result.get("time_connect", 0):
        result["stage"] = "tcp_connect_or_dns"
    elif not result.get("time_appconnect", 0):
        result["stage"] = "tls"
    else:
        result["stage"] = "response_or_redirect"
    return result


def checkpoint(report, output):
    output.seek(0)
    json.dump(report, output, ensure_ascii=False, indent=2)
    output.write("\n")
    output.truncate()
    output.flush()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--label", required=True, choices=["openvpn", "sgate", "direct"])
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--reference", type=Path, help="also test the first IPv4 from each host in this earlier report")
    parser.add_argument("--endpoint", default="104.194.73.54", type=ipaddress.IPv4Address)
    parser.add_argument("--curl", default=shutil.which("curl"))
    parser.add_argument("--timeout", type=int, default=20)
    parser.add_argument("--repeat", type=int, default=1)
    parser.add_argument("--budget", type=int, default=90, help="total diagnostic budget in seconds, excluding final route snapshot")
    parser.add_argument("--download", action="store_true", help="add bounded public Gmail HTML transfers over HTTP/1.1 and HTTP/2 when supported")
    parser.add_argument("--download-only", action="store_true", help="test public Gmail HTML transfers without HEAD probes")
    args = parser.parse_args()
    if not args.curl or not 5 <= args.timeout <= 60 or not 1 <= args.repeat <= 3 or not 10 <= args.budget <= 300:
        parser.error("requires curl, timeout 5..60, repeat 1..3 and budget 10..300")
    # Reserve the output before network tests, with no overwrite and private permissions.
    deadline = time.monotonic() + args.budget
    descriptor = os.open(str(args.output), os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(descriptor, "w") as output:
        report = {"label": args.label, "started_utc": datetime.datetime.now(datetime.timezone.utc).isoformat(),
                  "platform": sys.platform, "scope": "Unauthenticated HTTPS only; no Gmail session, browser proxy, JS or mailbox validation",
                  "route_before": {"endpoint": route(str(args.endpoint)), "internet": route("1.1.1.1")},
                  "dns_overrides": dns_overrides(), "dns": {}, "probes": [], "completed": False}
        checkpoint(report, output)
        version = command([args.curl, "--version"])
        report["curl_version"] = version["stdout"].splitlines()[:1]
        features = set(re.findall(r"^Features:\s*(.*)$", version["stdout"], re.M)[0].split()) if "Features:" in version["stdout"] else set()
        protocols = ["http1.1"] + (["http2"] if "HTTP2" in features else []) + (["http3-only"] if "HTTP3" in features else [])
        report["protocols_tested"] = protocols
        report["http3_skipped"] = "HTTP3" not in features
        reference = json.loads(args.reference.read_text()) if args.reference else {}
        with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
            for host, value in zip(HOSTS, pool.map(resolve, HOSTS)):
                report["dns"][host] = value
        checkpoint(report, output)
        tasks = []
        for host in HOSTS:
            addresses = [None]
            prior = reference.get("dns", {}).get(host, {}).get("addresses", [])
            v4 = [str(ipaddress.IPv4Address(ip)) for ip in prior if ":" not in ip]
            if v4:
                addresses.append(v4[0])
            for address in addresses:
                for protocol in protocols:
                    for _ in range(args.repeat):
                        tasks.append((host, protocol, address, False))
        if args.download or args.download_only:
            if args.download_only:
                tasks = []
            # Perform body tests first, before HEAD probes consume the budget.
            tasks = [("mail.google.com", protocol, None, True) for protocol in protocols if protocol != "http3-only"] + tasks
        # Sequential requests avoid turning a diagnostic into concurrent load.
        for host, protocol, address, download in tasks:
            remaining = deadline - time.monotonic()
            if remaining < 1:
                report["stopped_reason"] = "diagnostic time budget exhausted; remaining probes not attempted"
                break
            result = probe(args.curl, host, protocol, address, min(args.timeout, remaining), download)
            report["probes"].append(result)
            checkpoint(report, output)
            print(result["kind"], host, protocol, address or "system DNS", "HTTP", result.get("http_code", "?"),
                  "exit", result["command_exitcode"], "stage", result["stage"], "bytes", result.get("size_download", 0),
                  "total", result.get("time_total", "?"), flush=True)
        report["completed"] = len(report["probes"]) == len(tasks)
        report["route_after"] = {"endpoint": route(str(args.endpoint)), "internet": route("1.1.1.1")}
        report["route_changed"] = report["route_before"] != report["route_after"]
        if report["route_changed"]:
            print("Route changed during the run; repeat with one VPN kept active.", file=sys.stderr)
        checkpoint(report, output)
    print("Saved", args.output)


if __name__ == "__main__":
    main()
