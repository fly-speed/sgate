#!/usr/bin/env python3
"""Real client process / authenticated Web VPN management regression.
Uses a deliberately invalid device name: no TUN or host network changes occur.
"""
import argparse
import http.cookiejar
import json
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.request

ROOT = Path(__file__).resolve().parents[1]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, default=ROOT / "bin/sgate-client")
    args = parser.parse_args()
    with tempfile.TemporaryDirectory(prefix="sgate-vpn-web-") as directory:
        root = Path(directory)
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0))
            port = listener.getsockname()[1]
        (root / "key").write_text(os.urandom(32).hex())
        config = root / "client.cf"
        config.write_text(f"""service isolated-vpn-web {{
 listen = 127.0.0.1:0
 key-file = {root}/key
 vpn-mode = only
 vpn-server = quic://127.0.0.1:8444/vpn
 vpn-tun-name = sgate-vpn-test-invalid-name-no-device
 proxies-file = {root}/must-not-be-created.json
 admin_enabled = true
 admin_listen = 127.0.0.1:{port}
 admin_web_dir = {ROOT}/web/admin
 admin_users_file = {root}/users.json
 admin_session_dir = {root}/sessions
 admin_setup_token_file = {root}/setup.token
}}
""")
        jar = http.cookiejar.CookieJar()
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), urllib.request.HTTPCookieProcessor(jar))
        base = f"http://127.0.0.1:{port}"
        csrf = ""

        def call(path, data=None, method=None):
            request = urllib.request.Request(base + "/api/" + path,
                                             data=None if data is None else json.dumps(data).encode(), method=method)
            request.add_header("Content-Type", "application/json")
            request.add_header("Origin", base)
            if csrf:
                request.add_header("X-CSRF-Token", csrf)
            with opener.open(request, timeout=10) as response:
                return json.load(response)

        def eventually(check):
            deadline = time.monotonic() + 15
            while True:
                try:
                    value = check()
                    if value:
                        return value
                except (OSError, urllib.error.URLError):
                    pass
                if time.monotonic() > deadline:
                    raise RuntimeError("management test timed out; " + (root / "process.log").read_text()[-3000:])
                time.sleep(0.05)

        with (root / "process.log").open("w") as log:
            process = subprocess.Popen([str(args.binary.resolve()), "-alone", "-f", str(config)], cwd=root, stdout=log, stderr=log)
            try:
                eventually(lambda: call("status").get("setup_required"))
                call("setup", {"username": "vpn-test", "password": "vpn-web-test-password",
                               "setup_token": (root / "setup.token").read_text().strip()})
                status = call("status")
                assert status["authenticated"]
                csrf = status["csrf"]
                view = call("config")
                assert any(field["key"] == "vpn-dns" for field in view["fields"])
                eventually(lambda: call("stats")["health"]["vpn"]["state"] == "failed")
                assert not (root / "must-not-be-created.json").exists()
                pid = call("config")["runtime"]["pid"]
                view = call("config")
                # Invalid MTU must be rejected without changing disk or live state.
                original = config.read_bytes()
                try:
                    call("config", {"revision": view["revision"], "fields": {"vpn-mtu": "50"}}, "PUT")
                    raise AssertionError("invalid MTU accepted")
                except urllib.error.HTTPError as error:
                    assert error.code == 400
                assert config.read_bytes() == original
                # Save a changed VPN option without applying it.
                call("config", {"revision": view["revision"], "fields": {"vpn-mtu": "1200"}}, "PUT")
                # Saved configuration is pending: failed runtime has not silently changed.
                assert call("config")["requires_restart"]
                assert call("stats")["health"]["vpn"]["state"] == "failed"
                # Reapply only-mode with changed MTU: same process, independent new generation.
                view = call("config")
                before = call("stats")["health"]["vpn"]["generation"]
                call("config", {"revision": view["revision"], "fields": {"vpn-mode": "only", "vpn-mtu": "1280"}, "apply": True}, "PUT")
                eventually(lambda: call("stats")["health"]["vpn"]["state"] == "failed")
                assert call("stats")["health"]["vpn"]["generation"] != before
                assert call("config")["runtime"]["pid"] == pid
                view = call("config")
                content = config.read_text().replace("vpn-mode = only", "vpn-mode = disabled")
                content = content.replace(f"proxies-file = {root}/must-not-be-created.json", "proxies-file = ''")
                content = content.replace("\n}", "\n server = ws://127.0.0.1:9/tunnel\n password = vpn-web-proxy-test\n}")
                call("config", {"revision": view["revision"], "content": content, "apply": True}, "PUT")
                assert call("stats")["health"]["vpn"]["state"] == "disabled"
                assert call("config")["runtime"]["pid"] == pid
                print("PASS: real client, authenticated VPN fields, invalid input, pending save, apply/retry, disable, failure visibility; no TUN created")
            finally:
                process.terminate()
                try:
                    process.wait(timeout=15)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()
                if process.returncode:
                    raise RuntimeError((root / "process.log").read_text()[-3000:])


if __name__ == "__main__":
    main()
