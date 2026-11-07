#!/usr/bin/env python3
"""Check real acl_master startup acknowledgements in isolated WS/WSS services."""
import argparse
import concurrent.futures
import contextlib
import grp
import importlib.util
import json
import os
import pathlib
import pwd
import re
import shutil
import signal
import socket
import socketserver
import subprocess
import tempfile
import threading
import time

ROOT = pathlib.Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("common", ROOT / "scripts/test-acl-master.py")
common = importlib.util.module_from_spec(spec)
spec.loader.exec_module(common)


class Echo(socketserver.BaseRequestHandler):
    def handle(self):
        with contextlib.suppress(OSError):
            while True:
                data = self.request.recv(65536)
                if not data:
                    return
                self.request.sendall(data)


class Backend(socketserver.ThreadingTCPServer):
    daemon_threads = True
    request_queue_size = 512


def run(args, secure):
    output = pathlib.Path(args.output).resolve() / ("wss" if secure else "ws")
    output.mkdir(parents=True, exist_ok=False)
    with tempfile.TemporaryDirectory(prefix="sgate-ready-") as directory, Backend(("127.0.0.1", 0), Echo) as backend:
        root = pathlib.Path(directory)
        for name in ["conf/services", "bin", "var/public", "var/pid", "work"]:
            (root / name).mkdir(parents=True)
        thread = threading.Thread(target=backend.serve_forever)
        thread.start()
        key = root / "key"
        key.write_text(os.urandom(32).hex())
        cert, tlskey = root / "cert.pem", root / "tls.key"
        if secure:
            subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
                            "-subj", "/CN=localhost", "-addext", "subjectAltName=IP:127.0.0.1,DNS:localhost", "-keyout", str(tlskey), "-out", str(cert)],
                           check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        ports = {name: common.port() for name in ["server", "client", "client2"]}
        socketpath = str(root / "var/public/master.sock")
        user, group = pwd.getpwuid(os.getuid()).pw_name, grp.getgrgid(os.getgid()).gr_name
        common.write(root / "conf/main.cf", f"""owner_user = {user}
owner_group = {group}
master_owner = {user}
queue_directory = {root}/var
pid_file = {root}/var/pid/master.pid
log_file = {output}/master.log
service_directory = {root}/conf/services
service_file =
daemon_directory = {pathlib.Path(args.master).resolve().parent}
manage_addr = {socketpath}
waiting_on_stop = true
stop_kill = true
file_exts = .cf
""")
        configs = {}
        for role in ["server", "client"]:
            listeners = [ports[role]] + ([ports["client2"]] if role == "client" else [])
            application = f"key-file = {key}\npassword = isolated-startup-password\nnoise-interval-min = 10ms\nnoise-interval-max = 30ms\nnoise-min-bytes = 128\nnoise-max-bytes = 512\n"
            if role == "server":
                application += f"target = 127.0.0.1:{backend.server_address[1]}\n"
                if secure:
                    application += f"tls-cert = {cert}\ntls-key = {tlskey}\n"
            else:
                url = f"{'wss' if secure else 'ws'}://127.0.0.1:{ports['server']}/tunnel"
                application += "proxies = '" + json.dumps([{"listen": f"127.0.0.1:{p}", "server": url} for p in listeners]) + "'\n"
                if secure:
                    application += f"tls-ca = {cert}\n"
            common.write(root / f"conf/sgate-{role}.cf", f"""service ready-{role} {{
master_disable = no
master_type = sock
master_service = {', '.join('127.0.0.1|' + str(p) for p in listeners)}
master_maxproc = 1
master_prefork = 1
master_status_notify = 0
master_reuseport = no
master_unpriv = n
master_chroot = n
master_private = n
master_stop_kill = false
master_stop_wait = true
master_command = {getattr(args, role)}
master_home = {root}/work
master_stdout = {output}/{role}.stdout
master_stderr = {output}/{role}.stderr
app_use_limit = 0
app_idle_limit = 0
{application}}}
""")
            # Preserve the same literal identity used by the installed scripts.
            configs[role] = str(root) + f"/bin/../conf/sgate-{role}.cf"
        for action in ["start", "stop"]:
            shutil.copy2(ROOT / f"sgate-client-{action}.sh", root / f"bin/sgate-client-{action}.sh")
        environment = dict(os.environ, MASTER_CTL=args.ctl, MASTER_SOCKET=socketpath, MASTER_CTL_TIMEOUT="3000")
        results = []
        master = subprocess.Popen([args.master, "-c", str(root / "conf"), "-l", str(output / "master.log")],
                                  stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

        def control(role, action, ready=True):
            command = ([str(root / f"bin/sgate-client-{action}.sh")] if role == "client" else
                       [args.ctl, "-s", socketpath, "-f", configs[role], "-a", action, "-t", "3000"])
            started = time.monotonic()
            result = subprocess.run(command, env=environment, capture_output=True, text=True, timeout=10)
            elapsed = time.monotonic() - started
            with (output / "control.log").open("a") as file:
                file.write(f"{role} {action} {elapsed:.3f}s\n{result.stdout}{result.stderr}")
            assert result.returncode == 0, result.stderr
            statuses = re.findall(r"^status\s*:\s*(\d+)", result.stdout, re.M)
            assert statuses and all((s == "200") == ready for s in statuses), result.stdout
            results.append(dict(role=role, action=action, seconds=elapsed, statuses=statuses))

        def transfer(index):
            payload = os.urandom(16384)
            with socket.create_connection(("127.0.0.1", ports["client2" if index % 2 else "client"]), 5) as peer:
                peer.settimeout(10)
                peer.sendall(payload)
                received = bytearray()
                while len(received) < len(payload):
                    chunk = peer.recv(len(payload) - len(received))
                    assert chunk, "early EOF"
                    received.extend(chunk)
                assert received == payload, "data lost, duplicated or crossed streams"

        try:
            common.eventually(lambda: pathlib.Path(socketpath).exists(), 10)
            for cycle in range(5):
                control("server", "start")
                control("client", "start")
                with concurrent.futures.ThreadPoolExecutor(max_workers=128) as pool:
                    list(pool.map(transfer, range(128)))
                control("client", "stop")
                control("server", "stop")
                # stop accepts the request before children finish closing sockets.
                time.sleep(0.25)
            # Initialization failure must never acknowledge startup readiness.
            server = pathlib.Path(configs["server"])
            text = server.read_text().replace(
                f"target = 127.0.0.1:{backend.server_address[1]}", "target =")
            common.write(server, text)
            control("server", "start", ready=False)
            control("server", "stop")
            result = dict(status="passed", protocol="wss" if secure else "ws", cycles=5,
                          concurrency=128, startup_failure_rejected=True, control_results=results)
        finally:
            master.send_signal(signal.SIGTERM)
            master.wait(15)
            backend.shutdown()
            thread.join(5)
        for number in ports.values():
            with socket.socket() as probe:
                probe.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
                probe.bind(("127.0.0.1", number))
        result["ports_released"] = True
        (output / "result.json").write_text(json.dumps(result, indent=2) + "\n")
        return result


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ["client", "server", "output"]:
        parser.add_argument("--" + name, required=True)
    parser.add_argument("--master", default="/opt/soft/acl-master/libexec/acl_master")
    parser.add_argument("--ctl", default="/opt/soft/acl-master/bin/master_ctl")
    args = parser.parse_args()
    for role in ["client", "server"]:
        setattr(args, role, str(pathlib.Path(getattr(args, role)).resolve()))
    for secure in [False, True]:
        print(json.dumps(run(args, secure)), flush=True)
