#!/usr/bin/env python3
"""Audit every deployed form field against real client/server validation and storage."""
import argparse
import importlib.util
import json
import pathlib
import subprocess

ROOT = pathlib.Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("noise", ROOT / "scripts/test-server-noise.py")
noise = importlib.util.module_from_spec(spec)
spec.loader.exec_module(noise)
multi = noise.multi


def entries(text):
    return {
        line.split("=", 1)[0].strip(): line.split("=", 1)[1].strip()
        for line in text.splitlines()
        if "=" in line and not line.lstrip().startswith("#")
    }


class Case(noise.Case):
    def setup(self):
        super().setup()
        for role, path in self.configs.items():
            template = (
                ROOT / ("sgate-server.cf" if role == "server" else "sgate-client.cf")
            ).read_text()
            current = entries(path.read_text())
            additions = []
            for key, value in entries(template).items():
                if key in current:
                    continue
                if key == "master_log":
                    value = str(self.output / (role + "-service.log"))
                elif key.startswith("master_check_"):
                    value = "false"
                elif key in [
                    "admin-config",
                    "addr",
                    "tls-cert",
                    "tls-key",
                    "tls-ca",
                    "admin_tls_cert",
                    "admin_tls_key",
                ]:
                    value = ""
                additions.append(key + " = " + value)
            multi.write(path, path.read_text().replace("\n}", "\n" + "\n".join(additions) + "\n}"))
        if not self.cert.exists():
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
                    "-subj",
                    "/CN=localhost",
                    "-keyout",
                    str(self.root / "tls.key"),
                    "-out",
                    str(self.cert),
                ],
                check=True,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
            )

    def value(self, field, role):
        key, value, kind = field["key"], field["value"], field["validation"]["type"]
        if key == "password":
            return "isolated-config-audit-password"
        if key == "master_args":
            return "-noise-min-bytes 256"
        if kind == "integer":
            return "+" + value.lstrip("+")
        if kind == "duration":
            return {
                "heartbeat": "2s",
                "heartbeat-timeout": "60s",
                "reconnect-min": "500ms",
                "reconnect-max": "11s",
                "session-ttl": "7s",
                "offline-timeout": "7s",
                "admin_session_timeout": "2h",
            }.get(key, "0s" if value in ["0", "0s"] else "2h")
        if kind == "bytes":
            return "1K" if key == "noise-min-bytes" else "1M"
        if kind == "boolean":
            return next((v for v in field["validation"]["choices"] if v != value), value)
        if kind == "listen":
            host, port = value.rsplit(":", 1)
            return host + ":0" + port
        if kind == "address":
            return value.replace("127.0.0.1:", "localhost:") if value else self.targets["A"]
        if kind == "addresses":
            return self.targets["A"]
        if kind == "url":
            return value + ("&" if "?" in value else "?") + "keep=config-audit"
        if kind == "route":
            return "/audit-tunnel"
        if kind == "file" and value:
            path = pathlib.Path(value)
            return str(path.parent) + "/./" + path.name
        if key == "tls-ca":
            return str(self.cert)
        if kind == "seconds":
            return "0"
        return value

    def audit(self, admin, role):
        original = self.configs[role].read_text()
        config = admin.call("config")
        runtime = admin.runtime()
        tested = []
        for field in config["fields"]:
            if field["key"] == "proxies":
                payload = {"fields": {}, "proxies": field.get("mappings", [])}
            else:
                payload = {"fields": {field["key"]: self.value(field, role)}}
            payload.update(revision=admin.call("config")["revision"], apply=False)
            admin.call("config", payload, "PUT")
            assert (
                admin.runtime()["pid"] == runtime["pid"]
                and admin.runtime()["generation"] == runtime["generation"]
            )
            saved = admin.call("config")
            admin.call(
                "config",
                {"content": original, "revision": saved["revision"], "apply": False},
                "PUT",
            )
            assert self.configs[role].read_text() == original
            admin.authenticated()
            tested.append(field["key"])
        for cert, key in (
            [("tls-cert", "tls-key")] if role == "server" else [("admin_tls_cert", "admin_tls_key")]
        ):
            admin.call(
                "config",
                {
                    "fields": {cert: str(self.cert), key: str(self.root / "tls.key")},
                    "revision": admin.call("config")["revision"],
                    "apply": False,
                },
                "PUT",
            )
            admin.call(
                "config",
                {"content": original, "revision": admin.call("config")["revision"], "apply": False},
                "PUT",
            )
        for fields in [
            {"noise-interval": "1s", "noise-interval-min": "1s", "noise-interval-max": "3s"},
            {"master_args": "-unknown-option"},
            {"noise-max-bytes": "11M"},
        ]:
            admin.call(
                "config",
                {"fields": fields, "revision": admin.call("config")["revision"], "apply": False},
                "PUT",
                status=400,
            )
            assert self.configs[role].read_text() == original
        return tested

    def run(self):
        self.start()
        self.client_admin.call(
            "setup",
            {
                "username": "config-audit-admin",
                "password": "isolated-config-audit-password",
                "setup_token": (self.root / "client-setup.token").read_text().strip(),
            },
        )
        self.client_admin.authenticated()
        baseline = self.census("baseline")
        tested = {
            "server": self.audit(self.admin, "server"),
            "client0": self.audit(self.client_admin, "client0"),
        }
        self.wave(0)
        final = self.census("final-idle")
        for role in self.roles:
            assert final[role]["goroutines"] <= baseline[role]["goroutines"] + 8
        return dict(
            status="passed",
            mode=self.args.mode,
            protocol=self.args.tls,
            fields=tested,
            waves=self.waves,
            samples=self.samples,
            login_persistent=True,
            invalid_values_rollback=True,
            original_config_restored=True,
        )


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ["client", "server", "output"]:
        parser.add_argument("--" + name, required=True)
    parser.add_argument("--mode", choices=["standalone", "managed"], required=True)
    parser.add_argument("--tls", choices=["ws", "wss"], required=True)
    parser.add_argument("--master", default="/opt/soft/acl-master/libexec/acl_master")
    parser.add_argument("--ctl", default="/opt/soft/acl-master/bin/master_ctl")
    args = parser.parse_args()
    args.selection, args.records = "url", 128
    for name in ["client", "server"]:
        setattr(args, name, str(pathlib.Path(getattr(args, name)).resolve()))
    case, result = Case(args), {"status": "failed"}
    try:
        result = case.run()
    except BaseException as error:
        result["error"] = repr(error)
        case.snapshot(str(error))
        raise
    finally:
        try:
            result.update(case.close())
        except BaseException as error:
            result.update(status="failed", cleanup_error=repr(error))
            raise
        finally:
            (case.output / "result.json").write_text(json.dumps(result, indent=2) + "\n")
    print(result["status"])


if __name__ == "__main__":
    main()
