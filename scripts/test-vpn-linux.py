#!/usr/bin/env python3
"""Isolated Linux TUN/NAT VPN test. Creates three private network namespaces;
never changes host routes, forwarding, DNS or firewall rules. Requires root.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import shutil
import signal
import subprocess
import sys
import time
import traceback
import uuid


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, default=Path("bin/sgate-vpn"))
    parser.add_argument("--client-binary", type=Path, help="integrated sgate-client; requires --server-binary")
    parser.add_argument("--server-binary", type=Path, help="integrated sgate-server; requires --client-binary")
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--delay-ms", type=int, default=0, help="one-way outer-link delay")
    parser.add_argument("--loss-percent", type=float, default=0)
    args = parser.parse_args()
    needed = ["ip", "iptables", "sysctl", "openssl", "ping"]
    if args.delay_ms or args.loss_percent:
        needed.append("tc")
    if (platform.system() != "Linux" or os.geteuid() != 0 or
            any(not shutil.which(command) for command in needed) or
            not Path("/dev/net/tun").exists()):
        parser.error("requires Linux, root, /dev/net/tun, ip, iptables, sysctl, openssl, ping (tc for impairments); nothing changed")
    if bool(args.client_binary) != bool(args.server_binary):
        parser.error("provide both integrated binaries together")
    binaries = [args.client_binary, args.server_binary] if args.client_binary else [args.binary]
    if any(not binary.is_file() or not os.access(binary, os.X_OK) for binary in binaries):
        parser.error("build the Linux VPN executables first")
    if not 0 <= args.delay_ms <= 2000 or not 0 <= args.loss_percent <= 10:
        parser.error("invalid delay or loss")
    binary = args.binary.resolve()
    out = args.output.resolve()
    out.mkdir(parents=True, exist_ok=False, mode=0o700)
    token = "sgvpn-" + uuid.uuid4().hex[:8]
    client, server, internet = [token + suffix for suffix in ("-c", "-s", "-i")]
    created, processes, handles = [], [], []
    result = dict(status="failed", platform=platform.platform(), namespaces=[client, server, internet],
                  binaries_sha256={str(path): hashlib.sha256(path.read_bytes()).hexdigest() for path in binaries},
                  mode="integrated" if args.client_binary else "standalone",
                  delay_ms=args.delay_ms, loss_percent=args.loss_percent,
                  scope="Private namespaces, real TUN, QUIC DATAGRAM, TCP/UDP/ICMP, NAT and shutdown cleanup")
    env = {key: value for key, value in os.environ.items() if key.lower() not in
           ("http_proxy", "https_proxy", "all_proxy")}

    def run(command, namespace=None, timeout=15):
        full = (["ip", "netns", "exec", namespace] if namespace else []) + list(map(str, command))
        process = subprocess.run(full, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout)
        if process.returncode:
            raise RuntimeError(f"{full}: {process.stderr.decode(errors='replace')[-2000:]}")
        return process.stdout

    def start(name, command, namespace):
        log = (out / (name + ".log")).open("wb")
        handles.append(log)
        process = subprocess.Popen(["ip", "netns", "exec", namespace] + list(map(str, command)),
                                   env=env, stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
        processes.append(process)
        return process

    def stop(process):
        if process.poll() is None:
            try:
                os.killpg(process.pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                os.killpg(process.pid, signal.SIGKILL)
                process.wait()
                raise RuntimeError("VPN did not shut down within 10 seconds")
        if process.returncode:
            raise RuntimeError(f"VPN exited with {process.returncode}")

    try:
        for ns in (client, server, internet):
            entry = Path("/var/run/netns") / ns
            if entry.exists() or entry.is_symlink():
                raise RuntimeError("refusing to reuse an existing namespace")
            created.append(ns)  # Also clean partial creation failures.
            run(["ip", "netns", "add", ns])
            run(["ip", "link", "set", "lo", "up"], ns)
        run(["ip", "link", "add", "uplink", "type", "veth", "peer", "name", "uplink", "netns", server], client)
        run(["ip", "link", "add", "wan0", "type", "veth", "peer", "name", "uplink", "netns", internet], server)
        for ns, interface, address in [(client, "uplink", "192.0.2.2/24"),
                                       (server, "uplink", "192.0.2.1/24"),
                                       (server, "wan0", "198.51.100.1/24"),
                                       (internet, "uplink", "198.51.100.2/24")]:
            run(["ip", "addr", "add", address, "dev", interface], ns)
            run(["ip", "link", "set", interface, "up"], ns)
        for ns, gateway in [(client, "192.0.2.1"), (server, "198.51.100.2"), (internet, "198.51.100.1")]:
            run(["ip", "route", "add", "default", "via", gateway], ns)
        if args.delay_ms or args.loss_percent:
            for ns in (client, server):
                command = ["tc", "qdisc", "add", "dev", "uplink", "root", "netem", "limit", "10000"]
                if args.delay_ms:
                    command += ["delay", str(args.delay_ms) + "ms"]
                if args.loss_percent:
                    command += ["loss", str(args.loss_percent) + "%"]
                run(command, ns)
        initial_forward = run(["sysctl", "-n", "net.ipv4.ip_forward"], server).strip()
        (out / "key").write_text(os.urandom(32).hex() + "\n")
        (out / "key").chmod(0o600)
        config = out / "openssl.cnf"
        config.write_text("[req]\nprompt=no\ndistinguished_name=dn\nx509_extensions=ext\n"
                          "[dn]\nCN=localhost\n[ext]\nsubjectAltName=IP:192.0.2.1\n"
                          "basicConstraints=critical,CA:TRUE\nkeyUsage=digitalSignature,keyCertSign\n")
        run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
             "-config", config, "-keyout", out / "tls-key.pem", "-out", out / "tls-cert.pem"])
        (out / "tls-key.pem").chmod(0o600)
        payload = os.urandom(2 * 1024 * 1024)
        http_directory = out / "http"
        http_directory.mkdir()
        (http_directory / "payload").write_bytes(payload)
        start("http", [sys.executable, "-m", "http.server", "18080", "--bind", "198.51.100.2", "--directory", http_directory], internet)
        echo = "import socket; s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); s.bind(('198.51.100.2',18081))\nwhile True:\n p,a=s.recvfrom(65535); s.sendto(p,a)"
        start("udp", [sys.executable, "-c", echo], internet)
        if args.client_binary:
            server_command = [args.server_binary.resolve(), "-alone", "-vpn-mode", "only", "-listen", "127.0.0.1:0",
                              "-key-file", out / "key", "-vpn-listen", "192.0.2.1:8444",
                              "-vpn-tls-cert", out / "tls-cert.pem", "-vpn-tls-key", out / "tls-key.pem",
                              "-vpn-egress-interface", "wan0"]
            client_command = [args.client_binary.resolve(), "-alone", "-vpn-mode", "only", "-listen", "127.0.0.1:0",
                              "-key-file", out / "key", "-vpn-server", "quic://192.0.2.1:8444/vpn",
                              "-vpn-tls-ca", out / "tls-cert.pem", "-vpn-dns", ""]
        else:
            server_command = [binary, "-role", "server", "-listen", "192.0.2.1:8444", "-key-file", out / "key",
                              "-tls-cert", out / "tls-cert.pem", "-tls-key", out / "tls-key.pem", "-egress-interface", "wan0"]
            client_command = [binary, "-role", "client", "-server", "quic://192.0.2.1:8444/vpn",
                              "-key-file", out / "key", "-tls-ca", out / "tls-cert.pem", "-dns", ""]
        server_process = start("server", server_command, server)
        client_process = start("client", client_command, client)
        deadline = time.monotonic() + 30
        while True:
            try:
                route = run(["ip", "-4", "route", "get", "198.51.100.2"], client).decode()
                if "sgate0" not in route:
                    raise RuntimeError("client VPN route not ready")
                run(["ping", "-c", "1", "-W", "3", "10.77.0.1"], client)
                break
            except (RuntimeError, subprocess.TimeoutExpired):
                if time.monotonic() > deadline or client_process.poll() is not None or server_process.poll() is not None:
                    raise
                time.sleep(0.2)
        result["client_route"] = route
        endpoint_route = run(["ip", "route", "get", "192.0.2.1"], client).decode()
        if "sgate0" in endpoint_route:
            raise RuntimeError("VPN endpoint routes recursively into VPN")
        fetch = "import urllib.request,sys; sys.stdout.buffer.write(urllib.request.urlopen('http://198.51.100.2:18080/payload',timeout=30).read())"
        actual = run([sys.executable, "-c", fetch], client, timeout=40)
        if actual != payload:
            raise RuntimeError("TCP payload mismatch")
        udp_test = "import socket; s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); s.settimeout(5)\nfor n in [64,1000,1200]:\n p=bytes(i%251 for i in range(n)); s.sendto(p,('198.51.100.2',18081)); assert s.recv(65535)==p\nprint('UDP verified')"
        run([sys.executable, "-c", udp_test], client)
        run(["ping", "-c", "3", "-W", "5", "198.51.100.2"], client)
        stop(client_process)
        routes = run(["ip", "route", "show"], client).decode()
        if "sgate0" in routes or "0.0.0.0/1" in routes or "128.0.0.0/1" in routes:
            raise RuntimeError("client routes not cleaned up")
        stop(server_process)
        if b"SGATEVPN" in run(["iptables", "-S"], server) or b"SGATEVPN" in run(["iptables", "-t", "nat", "-S"], server):
            raise RuntimeError("server firewall rules not cleaned up")
        if run(["sysctl", "-n", "net.ipv4.ip_forward"], server).strip() != initial_forward:
            raise RuntimeError("forwarding setting not restored")
        result.update(status="passed", tcp_bytes=len(payload), udp_sizes=[64, 1000, 1200], icmp=True,
                      endpoint_exclusion=True, route_cleanup=True, firewall_cleanup=True, forwarding_restored=True)
    except Exception as error:
        result["error"] = str(error)
        (out / "failure.txt").write_text(traceback.format_exc())
    finally:
        for process in reversed(processes):
            if process.poll() is None:
                try:
                    os.killpg(process.pid, signal.SIGTERM)
                except ProcessLookupError:
                    pass
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    os.killpg(process.pid, signal.SIGKILL)
                    process.wait()
        for ns in reversed(created):
            cleanup = subprocess.run(["ip", "netns", "del", ns], capture_output=True)
            if cleanup.returncode:
                result["status"] = "failed"
                result.setdefault("cleanup_errors", []).append(ns)
        for handle in handles:
            handle.close()
        (out / "verification.json").write_text(json.dumps(result, indent=2) + "\n")
    print(json.dumps(result, indent=2))
    return int(result["status"] != "passed")


if __name__ == "__main__":
    raise SystemExit(main())
