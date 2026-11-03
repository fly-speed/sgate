#!/usr/bin/env python3
"""Test role selection through the real Makefile without Go or root privileges."""
import pathlib
import shutil
import subprocess
import tempfile
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[1]


class InstallTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="sgate-install-")
        self.addCleanup(self.tmp.cleanup)
        self.root = pathlib.Path(self.tmp.name) / "source"
        self.dest = pathlib.Path(self.tmp.name) / "package"
        self.target = self.dest / "opt/soft/sgate"
        names = ["Makefile", "LICENSE", "scripts/install.sh", "samples/README.md", "docs/vpn.md", "scripts/test-vpn-linux.py"]
        names += ["samples/test-tls/" + name for name in ["README.md", "tls-cert.pem", "tls-key.pem"]]
        names += [str(p.relative_to(ROOT)) for p in (ROOT / "web/admin").iterdir() if p.is_file()]
        names += [str(p.relative_to(ROOT)) for p in ROOT.glob("sgate-*.cf")]
        names += [str(p.relative_to(ROOT)) for p in ROOT.glob("sgate-*-*.sh")]
        names += [str(p.relative_to(ROOT)) for p in (ROOT / "samples").glob("*.cf")]
        names += [str(p.relative_to(ROOT)) for p in (ROOT / "samples").glob("*.sh")]
        names += ["samples/sgate-vpn-client.conf", "samples/sgate-vpn-server.conf"]
        names += ["sgate-proxies.json", "samples/sgate-accounts.json", "samples/sgate-reverse-accounts.json", "samples/sgate-proxies.json"]
        names += ["third_party/" + role + "/LICENSE" for role in ["go-service", "gin-service"]]
        for name in names:
            dest = self.root / name
            dest.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(ROOT / name, dest)
        for role in ["client", "server"]:
            binary = self.root / "bin" / ("sgate-" + role)
            binary.parent.mkdir(exist_ok=True)
            binary.write_text("#!/bin/sh\nexit 0\n")
            binary.chmod(0o755)

    def install(self, *goals, success=True):
        result = subprocess.run(
            ["make", "-j4", "-C", str(self.root), *goals,
             "GO=/nonexistent/sgate-test-go", "PREFIX=/opt/soft/sgate", "DESTDIR=" + str(self.dest)],
            capture_output=True, text=True,
        )
        self.assertEqual(result.returncode == 0, success, result.stdout + result.stderr)
        return result

    def check_role(self, role):
        for name in ["sbin/sgate-" + role, "conf/sgate-" + role + ".cf",
                     "conf/sgate-" + role + ".cf.dist", "bin/sgate-" + role + "-start.sh",
                     "bin/sgate-" + role + "-stop.sh", "samples/ssh-" + role + ".cf",
                     "samples/vpn-" + role + ".cf"]:
            self.assertTrue((self.target / name).is_file(), name)
        for name in ["index.html", "app.css", "i18n.js", "app.js"]:
            self.assertEqual((self.target / "web/admin" / name).read_bytes(),
                             (self.root / "web/admin" / name).read_bytes())
        self.assertTrue((self.target / "conf/sgate.key").is_file())
        for name in ["tls-cert.pem", "tls-key.pem"]:
            self.assertTrue((self.target / "conf" / name).is_file())
            self.assertFalse((self.target / "samples/test-tls" / name).exists())
        self.assertTrue((self.target / "samples/test-tls/README.md").is_file())
        self.assertEqual((self.target / "conf/tls-key.pem").stat().st_mode & 0o777, 0o600)
        self.assertEqual((self.target / "conf").stat().st_mode & 0o777, 0o700)
        if role == "server":
            self.assertTrue((self.target / "samples/sgate-accounts.json").is_file())
            self.assertTrue((self.target / "samples/sgate-reverse-accounts.json").is_file())
        else:
            self.assertTrue((self.target / "samples/sgate-proxies.json").is_file())
            self.assertTrue((self.target / "conf/sgate-proxies.json").is_file())
            self.assertTrue((self.target / "conf/sgate-proxies.json.dist").is_file())

    def test_default_installs_both(self):
        self.install("install")
        self.check_role("client")
        self.check_role("server")

    def test_each_role_needs_only_its_own_inputs(self):
        for role, other in [("server", "client"), ("client", "server")]:
            with self.subTest(role=role):
                # Isolate each destination; remove the unselected executable and config.
                removed = [self.root / "bin" / ("sgate-" + other),
                           self.root / ("sgate-" + other + ".cf")]
                saved = [(p, p.read_bytes(), p.stat().st_mode) for p in removed]
                for p in removed:
                    p.unlink()
                self.install("install", role)
                self.check_role(role)
                self.assertFalse((self.target / "sbin" / ("sgate-" + other)).exists())
                self.assertFalse((self.target / "conf" / ("sgate-" + other + ".cf")).exists())
                self.assertFalse((self.target / "bin" / ("sgate-" + other + "-start.sh")).exists())
                self.assertFalse((self.target / "samples" / ("ssh-" + other + ".cf")).exists())
                shutil.rmtree(self.dest)
                for p, content, mode in saved:
                    p.write_bytes(content)
                    p.chmod(mode)

    def test_repeat_and_cross_role_install_preserve_user_state_and_other_program(self):
        self.install("install")
        keep = ["conf/sgate-client.cf", "conf/sgate-server.cf", "conf/sgate.key",
                "conf/tls-cert.pem", "conf/tls-key.pem",
                "conf/tunnel-accounts.json", "conf/sgate-proxies.json", "samples/sgate-accounts.json", "samples/sgate-reverse-accounts.json", "samples/sgate-proxies.json", "conf/admin-client-users.json", "conf/admin-server-sessions/session.json",
                "sbin/sgate-client", "bin/sgate-client-start.sh", "samples/ssh-client.cf"]
        for name in keep:
            p = self.target / name
            p.parent.mkdir(parents=True, exist_ok=True)
            p.write_text("preserve: " + name)
        before = {name: (self.target / name).read_bytes() for name in keep}
        self.install("install", "server")
        for name, content in before.items():
            self.assertEqual((self.target / name).read_bytes(), content, name)
        self.install("client", "install-files")
        for name in keep:
            if name.startswith(("conf/", "samples/")):
                self.assertEqual((self.target / name).read_bytes(), before[name], name)

    def test_independent_vpn_install_and_custom_paths(self):
        binary = self.root / "bin/sgate-vpn"
        binary.write_text("#!/bin/sh\nexit 0\n")
        binary.chmod(0o755)
        self.install("install", "vpn")
        self.assertTrue((self.target / "sbin/sgate-vpn").is_file())
        self.assertTrue((self.target / "docs/vpn.md").is_file())
        self.assertTrue((self.target / "bin/test-vpn-linux.py").is_file())
        self.assertFalse((self.target / "sbin/sgate-client").exists())
        self.assertFalse((self.target / "sbin/sgate-server").exists())
        for role in ["client", "server"]:
            self.assertTrue((self.target / f"samples/sgate-vpn-{role}.conf").is_file())
        result = subprocess.run(["make", "-C", str(self.root), "install-vpn", "PREFIX=/custom/sgate", "DESTDIR="+str(self.dest)], capture_output=True,text=True)
        self.assertEqual(result.returncode,0,result.stdout+result.stderr)
        config = (self.dest / "custom/sgate/samples/sgate-vpn-server.conf").read_text()
        self.assertIn('/custom/sgate/conf/tls-cert.pem', config)
        self.install("install")
        self.check_role("client")
        self.check_role("server")
        self.assertTrue((self.target / "sbin/sgate-vpn").is_file())

    def test_missing_selected_binary_does_not_create_destination(self):
        (self.root / "bin/sgate-server").unlink()
        result = self.install("install", "server", success=False)
        self.assertIn("make build", result.stderr)
        self.assertFalse(self.dest.exists())

    def test_partial_existing_tls_pair_is_not_mixed_with_bundled_files(self):
        directory = self.target / "conf"
        directory.mkdir(parents=True)
        cert = directory / "tls-cert.pem"
        cert.write_text("custom certificate awaiting its own key")
        self.install("install", "server")
        self.assertEqual(cert.read_text(), "custom certificate awaiting its own key")
        self.assertFalse((directory / "tls-key.pem").exists())

    def test_missing_test_tls_input_does_not_create_destination(self):
        (self.root / "samples/test-tls/tls-key.pem").unlink()
        self.install("install", "server", success=False)
        self.assertFalse(self.dest.exists())

    def test_standalone_build_targets_still_compile(self):
        for goal in ["client", "server", "build"]:
            result = subprocess.run(["make", "-n", "-C", str(self.root), goal], capture_output=True, text=True)
            self.assertEqual(result.returncode, 0)
            for role in ["client", "server"]:
                self.assertEqual("./cmd/sgate-" + role in result.stdout, goal in [role, "build"])


if __name__ == "__main__":
    unittest.main()
