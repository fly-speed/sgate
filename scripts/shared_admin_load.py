"""Concurrent management and framed application traffic for isolated tests."""

import concurrent.futures
import copy
import hashlib
import json
import pathlib
import re
import socket
import threading
import time
import urllib.error
import urllib.request


def planned_shutdown_response(error, planned):
    return (
        planned
        and isinstance(error, AssertionError)
        and len(error.args) == 1
        and isinstance(error.args[0], tuple)
        and len(error.args[0]) >= 2
        and error.args[0][1] == 503
    )


def run(
    args,
    output,
    admins,
    addresses,
    relay,
    ctl,
    pid,
    pid_gone,
    round_no,
    sessions,
    Admin,
    eventually,
    failure_probe=None,
):
    stopping = threading.Event()
    faults_done = threading.Event()
    probes_done = threading.Event()
    restarting = threading.Event()
    lock = threading.Lock()
    active, sockets = set(), set()
    errors, requests, outages = [], [], []
    counts = dict(
        records=0,
        checked_bytes=0,
        partial_bytes_at_reset=0,
        stream_resets=0,
        connect_failures=0,
        peak_streams=0,
        login_accepted=0,
        login_throttled=0,
        config_applies=0,
        config_saves=0,
        explicit_applies=0,
        process_restarts=0,
        path_updates=0,
        route_checks=0,
        planned_shutdown_responses=0,
        sustained_applies=0,
    )
    drops_before = relay.drops

    def clone(original):
        result = Admin(original.base, original.context)
        for cookie in original.jar:
            result.jar.set_cookie(copy.copy(cookie))
        result.csrf = original.csrf
        return result

    def call(client, path, data=None, method=None, status=200):
        begin = time.monotonic()
        planned = restarting.is_set()
        outcome = "ok"
        try:
            return client.call(path, data, method, status)
        except Exception as error:
            outcome = type(error).__name__
            raise
        finally:
            with lock:
                requests.append(
                    dict(
                        path=path,
                        seconds=time.monotonic() - begin,
                        outcome=outcome,
                        planned_restart=planned or restarting.is_set(),
                    )
                )

    def probe(original):
        client = clone(original)
        while not probes_done.is_set():
            planned = restarting.is_set()
            try:
                assert call(client, "status")["authenticated"], "persistent session revoked"
                call(client, "stats")
            except (OSError, urllib.error.URLError) as error:
                if not (planned or restarting.is_set()):
                    with lock:
                        errors.append(("management availability", str(error)))
            except AssertionError as error:
                # gin-service rejects new requests with plain-text 503 while
                # its worker drains. Only the explicit restart window permits
                # this response; 401/403 and malformed 200 remain failures.
                if planned_shutdown_response(error, planned or restarting.is_set()):
                    with lock:
                        counts["planned_shutdown_responses"] += 1
                else:
                    with lock:
                        errors.append(("management authentication", str(error)))
            except Exception as error:
                with lock:
                    errors.append(("management authentication", str(error)))
            probes_done.wait(0.04)

    def stream(index):
        attempt = 0
        while not stopping.is_set():
            attempt += 1
            connection = None
            received = bytearray()
            try:
                connection = socket.create_connection(
                    ("127.0.0.1", addresses["client"][index % len(addresses["client"])]), 5
                )
                connection.settimeout(10)
                with lock:
                    sockets.add(connection)
                sequence = 0
                while not stopping.is_set():
                    tag = f"round={round_no};stream={index};attempt={attempt};record={sequence};".encode()
                    payload = tag + hashlib.sha256(tag).digest() * 31
                    received = bytearray()
                    connection.sendall(payload)
                    while len(received) < len(payload):
                        chunk = connection.recv(len(payload) - len(received))
                        if not chunk:
                            raise ConnectionResetError("planned proxy/process reset")
                        received.extend(chunk)
                        assert received == payload[: len(received)], (
                            "duplicate/corrupt/cross-stream record",
                            index,
                            attempt,
                            sequence,
                        )
                    with lock:
                        counts["records"] += 1
                        counts["checked_bytes"] += len(payload)
                        active.add(index)
                        counts["peak_streams"] = max(counts["peak_streams"], len(active))
                    sequence += 1
                    stopping.wait(0.005)
            except (OSError, TimeoutError):
                with lock:
                    counts["partial_bytes_at_reset"] += len(received)
                    counts["stream_resets" if connection is not None else "connect_failures"] += 1
                stopping.wait(0.03)
            except Exception as error:
                with lock:
                    errors.append(("application integrity", str(error)))
                return
            finally:
                if connection is not None:
                    connection.close()
                    with lock:
                        sockets.discard(connection)
                with lock:
                    active.discard(index)

    def connected():
        with lock:
            assert not errors, errors
            return len(active) == args.concurrency

    def fault():
        # Plain builds may complete several applies before a delayed first
        # tick. Inject immediately so every mixed phase actually overlaps a
        # transport outage, including sub-second process replacements.
        while not faults_done.is_set():
            relay.drop()
            faults_done.wait(0.4)

    def apply(client, number):
        cfg = call(client, "config")
        content = cfg["content"] + f"\n# concurrent management round {round_no} apply {number}\n"
        content, changed = re.subn(
            r"(?m)^noise-max-bytes\s*=.*$",
            "noise-max-bytes = " + str(768 if number % 2 else 1024),
            content,
        )
        assert changed == 1, "random noise setting missing from editable configuration"
        if number % 2:
            call(
                client,
                "config",
                dict(content=content, revision=cfg["revision"], apply=False),
                "PUT",
            )
            counts["config_saves"] += 1
            call(client, "apply", {}, "POST")
            counts["explicit_applies"] += 1
        else:
            call(
                client, "config", dict(content=content, revision=cfg["revision"], apply=True), "PUT"
            )
            counts["config_saves"] += 1
        counts["config_applies"] += 1

    def login(index, barrier):
        client = Admin(admins["server"].base, admins["server"].context)
        barrier.wait(15)
        try:
            call(
                client,
                "login",
                dict(username="isolated-admin", password="isolated-management-password"),
            )
            client.authenticated()
            with lock:
                sessions.append(client)
                counts["login_accepted"] += 1
        except AssertionError as error:
            # Admission/rate limiting is part of the public login contract.
            if (
                len(error.args) == 1
                and isinstance(error.args[0], tuple)
                and error.args[0][0:2] == ("login", 429)
            ):
                with lock:
                    counts["login_throttled"] += 1
            else:
                raise

    def set_path(path):
        # Keep the listener, management prefix and CLI overrides unchanged.
        for role in ["server", "client"]:
            client = admins[role]
            before = client.runtime()

            def update(text):
                if role == "client":
                    result, changed = re.subn(
                        r"(?m)^(server\s*=\s*wss?://[^/\s]+)/[^\n]*$",
                        lambda match: match[1] + path,
                        text,
                    )
                    assert changed == 1, "client server URL missing"
                    return result
                text = re.sub(r"(?m)^path\s*=.*\n", "", text)
                end = text.rfind("}")
                assert end >= 0, "merged service block missing"
                return text[:end] + f"path = {path}\n" + text[end:]

            client.apply(update)
            after = client.runtime()
            assert after["pid"] == before["pid"]
            assert after["generation"] == before["generation"] + 1
            assert "noise-min-bytes" in after["cli_overrides"]
        counts["path_updates"] += 1

    def routes(current):
        # Use real HTTP requests with no login cookie for namespace boundaries.
        base = admins["server"].base.removesuffix("/admin")
        for path, method, wanted in [
            ("/admin/api/config", "GET", 401),
            ("/api/status", "GET", 404),
            ("/admin-other/api/status", "GET", 404),
            ("/admin/api/config", "POST", 401),
            (current, "GET", 404),
        ] + ([("/tunnel", "GET", 404)] if current != "/tunnel" else []):
            request = urllib.request.Request(base + path, method=method)
            try:
                response = urllib.request.urlopen(
                    request, context=admins["server"].context, timeout=10
                )
            except urllib.error.HTTPError as error:
                response = error
            with response:
                assert response.status == wanted, (path, method, response.status, wanted)
                response.read()
            counts["route_checks"] += 1
        call(admins["server"], "config", {}, "POST", status=405)
        counts["route_checks"] += 1

    def records_advance(previous):
        with lock:
            assert not errors, errors
            return counts["records"] >= previous + args.concurrency

    threads = [threading.Thread(target=stream, args=(i,)) for i in range(args.concurrency)]
    probes = [
        threading.Thread(target=probe, args=(admins[role],))
        for role in ["server", "server", "server", "client"]
    ]
    fault_thread = threading.Thread(target=fault)
    failure = None
    try:
        for thread in threads:
            thread.start()
        eventually(connected, 25)
        for thread in probes:
            thread.start()
        if failure_probe is not None:
            # Keep 128 streams and authenticated management probes active while
            # testing command failures; transport faults are layered on afterward.
            failure_probe()
        fault_thread.start()
        # Keep the same process and 128 workers busy long enough to expose
        # accumulation; process replacement later must not hide a leak here.
        until = time.monotonic() + args.mixed_seconds
        next_apply = time.monotonic() + args.continuous_apply_seconds
        while time.monotonic() < until:
            with lock:
                assert not errors, errors
            if args.continuous_apply_seconds and time.monotonic() >= next_apply:
                before = admins["server"].runtime()
                apply(admins["server"], counts["sustained_applies"])
                after = admins["server"].runtime()
                assert after["pid"] == before["pid"]
                assert after["generation"] == before["generation"] + 1
                counts["sustained_applies"] += 1
                eventually(connected, 25)
                next_apply = time.monotonic() + args.continuous_apply_seconds
            time.sleep(min(0.1, max(0, until - time.monotonic())))
        # Same-PID resource tests reuse authenticated sessions after one burst.
        # Repeated bursts exercise the long-lived admission limiter instead of
        # proxy cleanup, and intentionally grow the persisted session store.
        login_count = 0 if args.no_process_restarts and round_no else args.login_concurrency
        barrier = threading.Barrier(login_count + 1)
        with concurrent.futures.ThreadPoolExecutor(max_workers=max(1, login_count)) as pool:
            logins = [pool.submit(login, i, barrier) for i in range(login_count)]
            barrier.wait(15)
            for i in range(args.apply_rounds):
                before = admins["server"].runtime()
                apply(admins["server"], i)
                after = admins["server"].runtime()
                assert (
                    after["pid"] == before["pid"]
                    and after["generation"] == before["generation"] + 1
                )
            for future in logins:
                future.result(15)
        if login_count:
            assert counts["login_accepted"] > 0, "all concurrent logins rejected"
        # Reset the client proxy too, while the same 128 traffic workers run.
        apply(admins["client"], 0)
        # Exercise immutable Gin routes while traffic, probes and transport
        # failures run. Near-admin and literal pattern characters cover NoRoute.
        current_path = ["/admin-other/tunnel", "/:literal", "/*literal"][round_no % 3]
        set_path(current_path)
        routes(current_path)
        with lock:
            previous_records = counts["records"]
        eventually(lambda: records_advance(previous_records), 25)
        # A reserved path must leave the persisted configuration unchanged.
        cfg = admins["server"].call("config")
        admins["server"].apply(
            lambda text: text.replace(f"path = {current_path}\n", "path = /admin/api/status\n"),
            400,
        )
        assert admins["server"].call("config")["content"] == cfg["content"]
        roles = ["server", "client"] if round_no % 2 == 0 else ["server"]
        if args.no_process_restarts:
            roles = []
        for role in roles:
            old = pid(role)
            restarting.set()
            start = time.monotonic()
            try:
                ctl(role, "restart")
                eventually(lambda: admins[role].authenticated() and pid(role) != old, 25)
                eventually(lambda: pid_gone(old), 20)
                counts["process_restarts"] += 1
            finally:
                outages.append(dict(role=role, seconds=time.monotonic() - start))
                restarting.clear()
        routes(current_path)
        with lock:
            previous_records = counts["records"]
        eventually(lambda: records_advance(previous_records), 25)
        set_path("/tunnel")
        routes("/tunnel")
        faults_done.set()
        fault_thread.join(5)
        eventually(connected, 25)
        # Every accepted login, including earlier rounds, must retain its
        # original cookie across proxy applies and actual process replacement.
        for client in sessions + list(admins.values()):
            assert call(client, "status")["authenticated"]
        assert relay.drops > drops_before, "no actual transport fault during mixed management load"
        with lock:
            assert not errors, errors
    except BaseException as error:
        failure = str(error)
        raise
    finally:
        faults_done.set()
        probes_done.set()
        stopping.set()
        if fault_thread.ident is not None:
            fault_thread.join(5)
        for thread in threads + probes:
            if thread.ident is not None:
                thread.join(12)
        remaining = [thread for thread in threads + probes + [fault_thread] if thread.is_alive()]
        with lock:
            for connection in list(sockets):
                try:
                    connection.shutdown(socket.SHUT_RDWR)
                except OSError:
                    pass
                connection.close()
        for thread in remaining:
            thread.join(2)
        assert not any(
            thread.is_alive() for thread in threads + probes + [fault_thread]
        ), "management/application test thread retained"
        durations = sorted(v["seconds"] for v in requests)
        report = dict(
            status="failed" if failure else "passed",
            failure=failure,
            concurrency=args.concurrency,
            counts=counts,
            transport_faults=relay.drops - drops_before,
            persistent_sessions=len(sessions),
            management_requests=len(requests),
            max_request_seconds=max(durations, default=0),
            p95_request_seconds=durations[int(0.95 * (len(durations) - 1))] if durations else 0,
            planned_outages=outages,
            requests=requests,
            errors=errors,
            reset_semantics="Proxy/process replacement deliberately closes old application streams. All received record prefixes and completed records are checked; fixed-payload fault-only waves separately require complete delivery.",
        )
        pathlib.Path(output, f"management-load-{round_no}.json").write_text(
            json.dumps(report, indent=2) + "\n"
        )
    return {k: v for k, v in report.items() if k not in ["requests", "errors"]}
