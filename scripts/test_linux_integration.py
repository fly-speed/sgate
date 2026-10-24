"""Check coverage accounting; these tests do not stand in for Linux execution."""

import importlib.util
import json
from pathlib import Path
import tempfile
import unittest


SPEC = importlib.util.spec_from_file_location(
    "linux_integration", Path(__file__).with_name("test-linux-integration.py")
)
RUNNER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(RUNNER)


class LinuxCoverageTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.output = Path(directory.name)
        for path, (mode, secure, stress) in RUNNER.expected_results().items():
            result = dict(status="passed", mode=mode, secure=secure)
            if stress:
                result.update(
                    ports_released=True,
                    closed_process_censuses=11,
                    web_service_restart=mode == "managed",
                    client_tcp_framework="go-service",
                    stress=dict(
                        concurrency=128,
                        rounds=2,
                        management_load=[dict(status="passed"), dict(status="passed")],
                        persistent_login=True,
                        idle_resources_bounded=True,
                        client_framework_idle_samples=[
                            dict(tcp_framework="go-service", tcp_connections=0) for _ in range(4)
                        ],
                    ),
                )
            target = self.output / path
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text(json.dumps(result))

    def test_both_transports_and_management_load_are_required(self):
        command = RUNNER.stress_command([], "managed", self.output, 6)
        self.assertEqual(command[command.index("--tls") + 1], "both")
        self.assertIn("--management-stress", command)
        self.assertIn("--web-service-restart", command)
        self.assertIn("--require-client-framework", command)
        standalone = RUNNER.stress_command([], "standalone", self.output, 6)
        self.assertNotIn("--web-service-restart", standalone)
        results = RUNNER.collect_results(self.output, 2)
        self.assertEqual(len(results), 24)

    def test_extra_result_cannot_replace_missing_ws_stress(self):
        original = self.output / "race-managed-stress/WS/result.json"
        replacement = self.output / "unrelated/result.json"
        replacement.parent.mkdir()
        original.rename(replacement)
        with self.assertRaisesRegex(RuntimeError, "scenario matrix mismatch"):
            RUNNER.collect_results(self.output, 2)

    def test_passed_label_cannot_hide_missing_management_or_cleanup(self):
        path = self.output / "plain-standalone-stress/standalone/WSS/result.json"
        original = json.loads(path.read_text())
        for field in ["management_load", "idle_resources_bounded"]:
            with self.subTest(field=field):
                result = json.loads(json.dumps(original))
                result["stress"].pop(field)
                path.write_text(json.dumps(result))
                with self.assertRaisesRegex(RuntimeError, "incomplete stress verification"):
                    RUNNER.collect_results(self.output, 2)

    def test_wrong_protocol_result_is_rejected(self):
        path = self.output / "race-managed-stress/WS/result.json"
        result = json.loads(path.read_text())
        result["secure"] = True
        path.write_text(json.dumps(result))
        with self.assertRaisesRegex(RuntimeError, "mismatched scenario"):
            RUNNER.collect_results(self.output, 2)

    def test_framework_identity_and_drained_connections_are_required(self):
        path = self.output / "race-managed-stress/WS/result.json"
        original = json.loads(path.read_text())
        for field in ["identity", "connections", "missing_samples"]:
            with self.subTest(field=field):
                result = json.loads(json.dumps(original))
                if field == "identity":
                    result["client_tcp_framework"] = "net"
                elif field == "connections":
                    result["stress"]["client_framework_idle_samples"][-1]["tcp_connections"] = 1
                else:
                    result["stress"].pop("client_framework_idle_samples")
                path.write_text(json.dumps(result))
                with self.assertRaisesRegex(RuntimeError, "incomplete stress verification"):
                    RUNNER.collect_results(self.output, 2)


if __name__ == "__main__":
    unittest.main()
