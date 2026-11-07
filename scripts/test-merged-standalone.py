#!/usr/bin/env python3
"""Single-file standalone deployment, with persistent management and WS/WSS."""
import argparse, concurrent.futures, contextlib, json, os, pathlib, signal, socket
import ssl, subprocess, tempfile, threading, sys

sys.dont_write_bytecode = True
from importlib.util import spec_from_file_location, module_from_spec

spec = spec_from_file_location("acl_test", pathlib.Path(__file__).with_name("test-acl-master.py"))
helper = module_from_spec(spec)
spec.loader.exec_module(helper)


def run(args, secure):
    out = pathlib.Path(args.output).resolve() / ("WSS" if secure else "WS")
    out.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="sgate-merged-") as directory:
        root = pathlib.Path(directory)
        (root / "work").mkdir()
        backend = socket.socket()
        backend.bind(("127.0.0.1", 0))
        backend.listen()
        backend.settimeout(0.1)
        stopped = threading.Event()
        lock = threading.Lock()
        peers = set()
        workers = []

        def echo(c):
            try:
                with c:
                    while True:
                        b = c.recv(65536)
                        if not b:
                            break
                        c.sendall(b)
            except OSError:
                pass
            finally:
                with lock:
                    peers.discard(c)

        def accept():
            while not stopped.is_set():
                try:
                    c, _ = backend.accept()
                    with lock:
                        peers.add(c)
                    t = threading.Thread(target=echo, args=(c,))
                    workers.append(t)
                    t.start()
                except socket.timeout:
                    pass
                except OSError:
                    break

        acceptor = threading.Thread(target=accept)
        acceptor.start()
        proxy = {r: helper.port() for r in ["client", "server"]}
        management = {r: helper.port() for r in proxy}
        helper.write(root / "key", os.urandom(32).hex())
        context = ssl.create_default_context()
        if secure:
            helper.write(
                root / "openssl.cnf",
                "[req]\ndistinguished_name=dn\nx509_extensions=ext\nprompt=no\n[dn]\nCN=localhost\n[ext]\nsubjectAltName=IP:127.0.0.1,DNS:localhost\nbasicConstraints=critical,CA:TRUE\n",
            )
            subprocess.run(
                [
                    "openssl",
                    "req",
                    "-x509",
                    "-newkey",
                    "rsa:2048",
                    "-nodes",
                    "-days",
                    "1",
                    "-keyout",
                    str(root / "tls.key"),
                    "-out",
                    str(root / "cert"),
                    "-config",
                    str(root / "openssl.cnf"),
                ],
                check=True,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
            )
            context.load_verify_locations(str(root / "cert"))
        children = {}
        admins = {}
        logs = {}

        def start(role):
            f = (out / (role + ".log")).open("ab")
            logs[role] = f
            option = "-f" if role == "client" else "-config"
            children[role] = subprocess.Popen(
                [
                    str(pathlib.Path(getattr(args, role)).resolve()),
                    "-alone",
                    option,
                    str(root / (role + ".cf")),
                    "-master_service",
                    f"127.0.0.1:{proxy[role]}",
                ],
                cwd=root / "work",
                stdout=f,
                stderr=f,
            )

        def stop(role):
            p = children.pop(role, None)
            if p:
                p.send_signal(signal.SIGINT)
                try:
                    p.wait(12)
                except subprocess.TimeoutExpired:
                    p.kill()
                    p.wait()
                    raise
                assert p.returncode == 0, (role, p.returncode)
                logs.pop(role).close()

        def transfer(index):
            payload = (f"unique-stream={index};".encode() + bytes(range(256))) * 128
            with socket.create_connection(("127.0.0.1", proxy["client"]), 5) as c:
                c.settimeout(10)
                c.sendall(payload)
                c.shutdown(socket.SHUT_WR)
                got = bytearray()
                while True:
                    b = c.recv(65536)
                    if not b:
                        break
                    got.extend(b)
                assert got == payload, ("duplex", index, len(got), len(payload))

        try:
            for role in ["server", "client"]:
                text = f"service standalone-{role} {{\nmaster_service = 127.0.0.1|1\nmaster_args =\nkey-file = {root}/key\npassword = standalone-secret\nnoise-interval-min = 10ms\nnoise-interval-max = 30ms\nnoise-min-bytes = 128\nnoise-max-bytes = 512\nadmin_enabled = true\nadmin_listen = 127.0.0.1:{management[role]}\nadmin_web_dir = {helper.ROOT}/web/admin\nadmin_users_file = {role}-users.json\nadmin_session_dir = {role}-sessions\nadmin_setup_token_file = {role}-setup.token\n"
                if role == "server":
                    text += f"target = 127.0.0.1:{backend.getsockname()[1]}\n"
                else:
                    text += f'server = {"wss" if secure else "ws"}://127.0.0.1:{proxy["server"]}/tunnel\n'
                if secure:
                    text += f"admin_tls_cert = {root}/cert\nadmin_tls_key = {root}/tls.key\n"
                    text += (
                        f"tls-cert = {root}/cert\ntls-key = {root}/tls.key\n"
                        if role == "server"
                        else f"tls-ca = {root}/cert\n"
                    )
                helper.write(root / (role + ".cf"), text + "}\n")
                start(role)
                a = helper.Admin(
                    (
                        f'{"https" if secure else "http"}://127.0.0.1:{proxy[role]}/admin'
                        if role == "server"
                        else f'{"https" if secure else "http"}://127.0.0.1:{management[role]}'
                    ),
                    context,
                )
                admins[role] = a
                helper.eventually(lambda: a.call("status")["setup_required"])
                a.call(
                    "setup",
                    {
                        "username": "standalone-admin",
                        "password": "standalone-admin-password",
                        "setup_token": (root / (role + "-setup.token")).read_text().strip(),
                    },
                )
                a.authenticated()
                assert "standalone-secret" not in a.call("config")["content"]
                assert "master_service" in a.runtime()["cli_overrides"]
            helper.eventually(lambda: transfer(-1) is None)
            for wave in range(3):
                with concurrent.futures.ThreadPoolExecutor(max_workers=32) as pool:
                    list(pool.map(transfer, range(wave * 32, (wave + 1) * 32)))
                for role, a in admins.items():
                    old = children[role].pid
                    if wave == 0:
                        a.apply()
                        assert children[role].pid == old
                        before = (root / (role + ".cf")).read_bytes()
                        a.apply(
                            lambda text: text.replace(
                                "admin_enabled = true", "admin_enabled = false"
                            ),
                            409,
                        )
                        assert (root / (role + ".cf")).read_bytes() == before
                    if wave == 1:
                        stop(role)
                        start(role)
                        helper.eventually(a.authenticated)
                        assert children[role].pid != old
                    a.authenticated()
                helper.eventually(lambda: transfer(-wave - 2) is None)
        finally:
            for role in list(children):
                stop(role)
            stopped.set()
            backend.close()
            acceptor.join(3)
            with lock:
                for c in peers:
                    with contextlib.suppress(OSError):
                        c.shutdown(socket.SHUT_RDWR)
                    c.close()
            for t in workers:
                t.join(3)
            assert not acceptor.is_alive() and not any(t.is_alive() for t in workers)
        for number in list(proxy.values()) + list(management.values()):
            with socket.socket() as s:
                s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
                s.bind(("127.0.0.1", number))
        for p in out.glob("*.log"):
            assert "WARNING: DATA RACE" not in p.read_text()
        result = dict(
            secure=secure,
            status="passed",
            different_cwd=True,
            single_cf=True,
            cli_override=True,
            duplex=True,
            web_apply=True,
            process_restart=True,
            persistent_login=True,
            ports_released=True,
        )
        (out / "result.json").write_text(json.dumps(result, indent=2) + "\n")
        return result


if __name__ == "__main__":
    p = argparse.ArgumentParser()
    p.add_argument("--client", required=True)
    p.add_argument("--server", required=True)
    p.add_argument("--output", required=True)
    args = p.parse_args()
    print(json.dumps([run(args, secure) for secure in [False, True]]))
