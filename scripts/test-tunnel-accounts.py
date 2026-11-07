#!/usr/bin/env python3
"""Exercise real standalone binaries with two accounts and 128 application sockets."""
import argparse
import concurrent.futures
import json
import pathlib
import socket
import subprocess
import tempfile
import threading
import time

ROOT = pathlib.Path(__file__).resolve().parents[1]


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def wait_port(port, process):
    deadline = time.monotonic() + 10
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise AssertionError("proxy exited during startup; inspect isolated test logs")
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=0.1):
                return
        except OSError:
            time.sleep(0.05)
    raise AssertionError("proxy startup timed out")


def exercise(client, server, secure, mapping_file=False, missing_mapping_file=False):
    with tempfile.TemporaryDirectory(prefix="sgate-accounts-") as temp:
        directory = pathlib.Path(temp)
        listener = socket.socket()
        listener.bind(("127.0.0.1", 0))
        listener.listen(256)
        listener.settimeout(0.1)
        stopped = threading.Event()
        workers, sockets = [], set()
        lock = threading.Lock()

        def echo(conn):
            try:
                while data := conn.recv(65536):
                    conn.sendall(data)
            except OSError:
                pass
            finally:
                conn.close()
                with lock:
                    sockets.discard(conn)

        def accept():
            while not stopped.is_set():
                try:
                    conn, _ = listener.accept()
                except socket.timeout:
                    continue
                except OSError:
                    return
                with lock:
                    sockets.add(conn)
                thread = threading.Thread(target=echo, args=(conn,))
                workers.append(thread)
                thread.start()

        accepting = threading.Thread(target=accept)
        accepting.start()
        processes, logs = [], []
        ports = [free_port() for _ in range(3)]
        try:
            key = directory / "key"
            key.write_text("39" * 32)
            key.chmod(0o600)
            accounts = directory / "accounts.json"
            accounts.write_text(json.dumps({"version": 1, "accounts": [
                {"username": name, "password": name + "-test-secret"}
                for name in ("alice", "bob")]}))
            accounts.chmod(0o600)
            tls_options = ""
            if secure:
                subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes",
                                "-keyout", str(directory / "tls.key"), "-out", str(directory / "tls.crt"),
                                "-days", "1", "-subj", "/CN=127.0.0.1", "-addext", "subjectAltName=IP:127.0.0.1"],
                               check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
                tls_options = f"tls-cert = {directory}/tls.crt\ntls-key = {directory}/tls.key\n"
            noise = "noise-interval-min = 5ms\nnoise-interval-max = 20ms\nnoise-min-bytes = 1K\nnoise-max-bytes = 4K\n"
            server_config = directory / "server.cf"
            server_config.write_text(f"master_service = 127.0.0.1:{ports[0]}\ntarget = 127.0.0.1:{listener.getsockname()[1]}\n"
                                     f"key-file = {key}\naccounts-file = accounts.json\n" + tls_options + noise)

            def launch(binary, config, *extra):
                log = (directory / (config.stem + ".log")).open("w+")
                logs.append(log)
                process = subprocess.Popen([str(binary), "-alone", "-config", str(config), *extra],
                                           cwd=tempfile.gettempdir(), stdout=log, stderr=log)
                processes.append(process)
                return process

            process = launch(server, server_config)
            wait_port(ports[0], process)
            if mapping_file:
                config = directory / "client.cf"
                routes = [{"listen": f"127.0.0.1:{ports[index]}",
                           "server": f"{'wss' if secure else 'ws'}://127.0.0.1:{ports[0]}/tunnel",
                           "username": name, "password": name + "-test-secret"}
                          for index, name in enumerate(("alice", "bob"), 1)]
                mappings = directory / "routes.json"
                if not missing_mapping_file:
                    mappings.write_text(json.dumps(routes, indent=2))
                    mappings.chmod(0o600)
                text = (f"key-file = {key}\nproxies-file = routes.json\n"
                        "username = unused-common-account\npassword = unused-common-secret\n" + noise)
                if missing_mapping_file:
                    text += "proxies = '" + json.dumps(routes, separators=(",", ":")) + "'\n"
                if secure:
                    text += f"tls-ca = {directory}/tls.crt\n"
                config.write_text(text)
                client_process = launch(client, config)
                for port in ports[1:]:
                    wait_port(port, client_process)
                if json.loads(mappings.read_text()) != routes:
                    raise AssertionError("mapping initialization changed configured routes")
                if mappings.stat().st_mode & 0o777 != 0o600:
                    raise AssertionError("mapping file permissions are not private")
            else:
                for index, name in enumerate(("alice", "bob"), 1):
                    config = directory / (name + ".cf")
                    text = (f"master_service = 127.0.0.1:{ports[index]}\nserver = {'wss' if secure else 'ws'}://127.0.0.1:{ports[0]}/tunnel\n"
                            f"key-file = {key}\nusername = alice\npassword = alice-test-secret\n" + noise)
                    if secure:
                        text += f"tls-ca = {directory}/tls.crt\n"
                    config.write_text(text)
                    extra = ("-username", name, "-password", name + "-test-secret") if name == "bob" else ()
                    wait_port(ports[index], launch(client, config, *extra))

            def exchange(index):
                payload = (f"account-{index % 2}-stream-{index}:".encode() * 8192)
                with socket.create_connection(("127.0.0.1", ports[1 + index % 2]), timeout=15) as conn:
                    conn.sendall(payload)
                    received = bytearray()
                    while len(received) < len(payload):
                        chunk = conn.recv(min(65536, len(payload) - len(received)))
                        if not chunk:
                            raise AssertionError("truncated account stream")
                        received.extend(chunk)
                    if received != payload:
                        raise AssertionError("mixed or corrupted account stream")
                    conn.settimeout(0.05)
                    try:
                        extra = conn.recv(1)
                    except socket.timeout:
                        extra = b""
                    if extra:
                        raise AssertionError("duplicated account payload")
                return 2 * len(payload)

            with concurrent.futures.ThreadPoolExecutor(max_workers=128) as pool:
                total = sum(pool.map(exchange, range(128)))
        finally:
            for process in reversed(processes):
                if process.poll() is None:
                    process.terminate()
                try:
                    process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()
                    raise AssertionError("proxy did not terminate")
            stopped.set()
            listener.close()
            accepting.join(timeout=2)
            with lock:
                remaining = list(sockets)
            for conn in remaining:
                conn.close()
            for thread in workers:
                thread.join(timeout=2)
            for log in logs:
                log.close()
        if accepting.is_alive() or any(thread.is_alive() for thread in workers):
            raise AssertionError("backend threads retained")
        for port in ports:
            with socket.socket() as sock:
                sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
                sock.bind(("127.0.0.1", port))
        return {"protocol": "wss" if secure else "ws", "accounts": 2,
                "concurrency": 128, "checked_duplex_bytes": total,
                "external_mapping_file": mapping_file, "mapping_file_created": missing_mapping_file,
                "client_processes": 1 if mapping_file else 2,
                "ports_released": True, "backend_threads_released": True}


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--client", type=pathlib.Path, default=ROOT / "bin/sgate-client")
    parser.add_argument("--server", type=pathlib.Path, default=ROOT / "bin/sgate-server")
    parser.add_argument("--mapping-file", action="store_true", help="use one client with a two-account external mapping file")
    parser.add_argument("--missing-mapping-file", action="store_true", help="initialize an absent mapping file from inline seed mappings")
    args = parser.parse_args()
    print(json.dumps([exercise(args.client.resolve(), args.server.resolve(), secure, args.mapping_file or args.missing_mapping_file, args.missing_mapping_file)
                      for secure in (False, True)], indent=2))
