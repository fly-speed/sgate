import contextlib
import importlib.util
import io
import json
from pathlib import Path
import tempfile
import types
import unittest
from unittest import mock


SPEC = importlib.util.spec_from_file_location(
    "parallel_diagnostic", Path(__file__).with_name("diagnose-network-memory-parallel.py")
)
DIAGNOSTIC = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(DIAGNOSTIC)


class ParallelSummaryTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.out = Path(self.directory.name)
        closed = dict.fromkeys((
            "backend_connections", "local_connections", "http_connections", "sessions",
            "streams", "pending_bytes", "nonces", "client_sessions", "client_streams",
            "client_pending_bytes", "relay_connections", "relay_queued_bytes", "transport_writers",
        ), 0)
        closed.update(phase="closed", goroutines=2, file_descriptors=6)
        for mode in ("control", "fragmented"):
            report = dict(tls=True, weak_network=True, independent_duplex=True,
                          clients=64, streams_per_client=4, session_retention="2m0s",
                          verified_upload_bytes=1024, verified_download_bytes=1024,
                          unconfirmed_release_milliseconds=120000, unconfirmed_mode=mode,
                          samples=[closed])
            (self.out / f"plain-{mode}.json").write_text(json.dumps(report))
        self.args = types.SimpleNamespace(streams=4, start_offset=2)

    @staticmethod
    def observation(processes=None):
        return dict(network_memory_denied=0, pool_failures={"mbuf": 0},
                    cluster_pools_kb={"16": {"in_use": 0, "allocated": 1}},
                    details={} if processes is None else {"processes": processes})

    def test_empty_warmup_and_shutdown_do_not_hide_live_measurements(self):
        samples = [self.observation(), self.observation({
            "101": {"tcp_descriptors": 100, "rss_bytes": 1024},
            "102": {"tcp_descriptors": 200, "rss_bytes": 2048},
        }), self.observation({})]
        with contextlib.redirect_stdout(io.StringIO()):
            result = DIAGNOSTIC.summarize_group(
                self.args, "plain", self.out, samples,
                {"101": "control", "102": "fragmented"}, [],
            )
        self.assertEqual(result["peak_tcp_descriptors"], 300)
        self.assertEqual(result["sampled_peak_combined_rss_bytes"], 3072)

    def test_four_processes_have_distinct_artifacts_and_all_are_summarized(self):
        cases = DIAGNOSTIC.workloads(4)
        self.assertEqual(len(set(label for label, _ in cases)), 4)
        for label, mode in cases:
            (self.out / f"plain-{label}.json").write_text(
                (self.out / f"plain-{mode}.json").read_text()
            )
        self.args.processes = 4
        processes = {str(pid): {"tcp_descriptors": 100, "rss_bytes": 1024}
                     for pid in range(101, 105)}
        with contextlib.redirect_stdout(io.StringIO()):
            result = DIAGNOSTIC.summarize_group(
                self.args, "plain", self.out,
                [self.observation(), self.observation(processes), self.observation({})],
                dict(zip(processes, [label for label, _ in cases])), [],
            )
        self.assertEqual(len(result["results"]), 4)
        self.assertEqual(result["peak_tcp_descriptors"], 400)
        self.assertEqual(result["sampled_peak_combined_rss_bytes"], 4096)

    def test_eight_processes_keep_every_case_and_measurement(self):
        self.check_parallel_measurements(8)

    def test_sixteen_processes_keep_every_case_and_measurement(self):
        self.check_parallel_measurements(16)

    def check_parallel_measurements(self, count):
        cases = DIAGNOSTIC.workloads(count)
        self.assertEqual(len(set(label for label, _ in cases)), count)
        self.assertEqual([mode for _, mode in cases].count("control"), count // 2)
        self.assertEqual([mode for _, mode in cases].count("fragmented"), count // 2)
        for label, mode in cases:
            (self.out / f"plain-{label}.json").write_text(
                (self.out / f"plain-{mode}.json").read_text()
            )
        self.args.processes = count
        processes = {str(pid): {"tcp_descriptors": 100, "rss_bytes": 1024}
                     for pid in range(101, 101 + count)}
        with contextlib.redirect_stdout(io.StringIO()):
            result = DIAGNOSTIC.summarize_group(
                self.args, "plain", self.out,
                [self.observation(), self.observation(processes), self.observation({})],
                dict(zip(processes, [label for label, _ in cases])), [],
            )
        self.assertEqual(len(result["results"]), count)
        self.assertEqual(result["peak_tcp_descriptors"], count * 100)
        self.assertEqual(result["sampled_peak_combined_rss_bytes"], count * 1024)

    def test_optional_vmmap_timeout_is_saved_without_aborting(self):
        path = self.out / "vmmap.txt"
        with mock.patch.object(DIAGNOSTIC.subprocess, "run", side_effect=
                               DIAGNOSTIC.subprocess.TimeoutExpired("vmmap", 5)):
            DIAGNOSTIC.capture_vmmap(123, path)
        self.assertIn("vmmap unavailable for test PID 123", path.read_text())
        self.assertIn("Started:", path.read_text())
        self.assertIn("Completed:", path.read_text())

    def test_socket_inventory_timeout_retains_rss_and_marks_tcp_unknown(self):
        child = types.SimpleNamespace(pid=101, poll=lambda: None)
        with mock.patch.object(DIAGNOSTIC.serial, "command", side_effect=[
            "101 32\n", DIAGNOSTIC.subprocess.TimeoutExpired("lsof", 10),
        ]):
            rows = DIAGNOSTIC.process_details([child])
        self.assertEqual(rows["101"]["rss_bytes"], 32*1024)
        self.assertIsNone(rows["101"]["tcp_descriptors"])
        self.assertIn("timed out", rows["101"]["tcp_telemetry_error"])

    def test_tcp_telemetry_distinguishes_address_families_and_time_wait(self):
        raw = "\n".join((
            "tcp4 0 0 127.0.0.1.51000 127.0.0.1.52000 ESTABLISHED",
            "tcp4 0 0 127.0.0.1.51001 127.0.0.1.52000 TIME_WAIT",
            "tcp6 0 0 ::1.51000 ::1.52000 ESTABLISHED",
            "tcp4 0 0 192.168.1.2.51002 1.2.3.4.52000 ESTABLISHED",
        ))
        with mock.patch.object(DIAGNOSTIC.serial, "command", return_value=raw):
            result = DIAGNOSTIC.tcp_states({52000})
        self.assertEqual(result["test_listener_related"], {"ESTABLISHED": 2, "TIME_WAIT": 1})
        self.assertEqual(result["loopback_ephemeral_port_counts"], {"127.0.0.1": 2, "::1": 1})
        self.assertEqual(result["loopback_ephemeral_ports_by_state"]["127.0.0.1"]["TIME_WAIT"], 1)

    def test_resume_rejects_a_different_backend_family(self):
        (self.out / "summary.json").write_text(json.dumps({"backend_host": "127.0.0.1", "runs": []}))
        with mock.patch.object(DIAGNOSTIC.sys, "argv", [
            "diagnose", "--backend-host", "::1", "--output", str(self.out),
        ]), mock.patch.object(DIAGNOSTIC.sys, "platform", "darwin"), \
                mock.patch.object(DIAGNOSTIC, "run_group") as run, \
                contextlib.redirect_stderr(io.StringIO()):
            with self.assertRaises(SystemExit) as caught:
                DIAGNOSTIC.main()
        self.assertEqual(caught.exception.code, 2)
        run.assert_not_called()

    def test_resume_rejects_a_different_cohort_settle(self):
        (self.out / "summary.json").write_text(json.dumps({"cohort_settle": "0s", "runs": []}))
        with mock.patch.object(DIAGNOSTIC.sys, "argv", [
            "diagnose", "--cohort-settle", "35s", "--output", str(self.out),
        ]), mock.patch.object(DIAGNOSTIC.sys, "platform", "darwin"), \
                mock.patch.object(DIAGNOSTIC, "run_group") as run, \
                contextlib.redirect_stderr(io.StringIO()):
            with self.assertRaises(SystemExit) as caught:
                DIAGNOSTIC.main()
        self.assertEqual(caught.exception.code, 2)
        run.assert_not_called()

    def test_failed_plain_run_does_not_skip_race_comparison(self):
        with mock.patch.object(DIAGNOSTIC.sys, "argv", [
            "diagnose", "--processes", "4", "--output", str(self.out),
        ]), mock.patch.object(DIAGNOSTIC.sys, "platform", "darwin"), \
                mock.patch.object(DIAGNOSTIC, "run_group", side_effect=[
                    RuntimeError("one child failed"), {"variant": "race"},
                ]) as run, contextlib.redirect_stdout(io.StringIO()):
            with self.assertRaises(SystemExit) as caught:
                DIAGNOSTIC.main()
        self.assertEqual(caught.exception.code, 1)
        self.assertEqual([call.args[1] for call in run.call_args_list], ["plain", "race"])
        failures = json.loads((self.out / "failures.json").read_text())
        self.assertEqual(failures[0]["variant"], "plain")
        summary = json.loads((self.out / "summary.json").read_text())
        self.assertEqual(summary["runs"], [{"variant": "race"}])

    def test_successful_retry_clears_its_previous_failure(self):
        failure_path = self.out / "failures.json"
        failure_path.write_text(json.dumps([{"variant": "plain", "error": "old failure"}]))
        with mock.patch.object(DIAGNOSTIC.sys, "argv", [
            "diagnose", "--processes", "4", "--variants", "plain", "--output", str(self.out),
        ]), mock.patch.object(DIAGNOSTIC.sys, "platform", "darwin"), \
                mock.patch.object(DIAGNOSTIC, "run_group", return_value={"variant": "plain"}), \
                contextlib.redirect_stdout(io.StringIO()):
            DIAGNOSTIC.main()
        self.assertFalse(failure_path.exists())

    def test_resume_rejects_a_different_parallel_count(self):
        (self.out / "summary.json").write_text(json.dumps({"parallel_processes": 2, "runs": []}))
        with mock.patch.object(DIAGNOSTIC.sys, "argv", [
            "diagnose", "--processes", "4", "--output", str(self.out),
        ]), mock.patch.object(DIAGNOSTIC.sys, "platform", "darwin"), \
                mock.patch.object(DIAGNOSTIC, "run_group") as run, \
                contextlib.redirect_stderr(io.StringIO()):
            with self.assertRaises(SystemExit) as caught:
                DIAGNOSTIC.main()
        self.assertEqual(caught.exception.code, 2)
        run.assert_not_called()

    def test_capacity_control_validates_actual_client_count(self):
        report = json.loads((self.out / "plain-control.json").read_text())
        report["clients"] = 32
        DIAGNOSTIC.validate(report, 4, clients=32)
        with self.assertRaises(AssertionError):
            DIAGNOSTIC.validate(report, 4)

    def test_socket_bound_covers_both_rounds_and_the_expiring_cohort(self):
        report = json.loads((self.out / "plain-control.json").read_text())
        report.update(socket_buffer_bytes=16384, backend_sockets_bound=64*4*3)
        DIAGNOSTIC.validate(report, 4)
        report["backend_sockets_bound"] = 64*4*2
        with self.assertRaisesRegex(AssertionError, "all three cohorts"):
            DIAGNOSTIC.validate(report, 4)

    def test_late_release_reports_the_observed_deadline(self):
        report = json.loads((self.out / "plain-control.json").read_text())
        report["unconfirmed_release_milliseconds"] = 125718
        with self.assertRaisesRegex(AssertionError, "125718ms outside 119000..125000ms"):
            DIAGNOSTIC.validate(report, 4)

    def test_resume_rejects_a_different_client_count(self):
        (self.out / "summary.json").write_text(json.dumps({"clients_per_process": 64, "runs": []}))
        with mock.patch.object(DIAGNOSTIC.sys, "argv", [
            "diagnose", "--processes", "16", "--clients", "32", "--output", str(self.out),
        ]), mock.patch.object(DIAGNOSTIC.sys, "platform", "darwin"), \
                mock.patch.object(DIAGNOSTIC, "run_group") as run, \
                contextlib.redirect_stderr(io.StringIO()):
            with self.assertRaises(SystemExit) as caught:
                DIAGNOSTIC.main()
        self.assertEqual(caught.exception.code, 2)
        run.assert_not_called()

    def test_duration_comparison_accepts_equivalent_go_spellings(self):
        self.assertEqual(DIAGNOSTIC.duration_ns("1m"), DIAGNOSTIC.duration_ns("1m0s"))
        self.assertEqual(DIAGNOSTIC.duration_ns(".25s"), DIAGNOSTIC.duration_ns("250ms"))
        self.assertEqual(DIAGNOSTIC.duration_ns("2m30s"), 150 * 10**9)
        for invalid in ("", "1m garbage", "garbage1s", "1"):
            with self.assertRaises(ValueError):
                DIAGNOSTIC.duration_ns(invalid)

    def test_synchronizer_requires_every_participant_once_per_round(self):
        labels = [label for label, _ in DIAGNOSTIC.workloads(16)]
        for label in labels[:-1]:
            (self.out / f"{label}-round-1.ready").write_text("512")
        self.assertEqual(DIAGNOSTIC.release_load_barriers(self.out, labels), [])
        (self.out / f"{labels[-1]}-round-1.ready").write_text("512")
        self.assertEqual(DIAGNOSTIC.release_load_barriers(self.out, labels), [1])
        self.assertEqual(DIAGNOSTIC.release_load_barriers(self.out, labels), [])
        for label in labels:
            (self.out / f"{label}-round-2.ready").write_text("512")
        self.assertEqual(DIAGNOSTIC.release_load_barriers(self.out, labels), [2])

    def test_full_scale_overlap_is_recorded_and_disjoint_load_is_rejected(self):
        self.args.start_offset = 0
        for label, ready, until in (("control", 1000, 2000), ("fragmented", 1500, 2500)):
            path = self.out / f"plain-{label}.json"
            report = json.loads(path.read_text())
            report["load_windows"] = [dict(round=1, streams=256, ready_unix_nano=ready, send_until_unix_nano=until)]
            path.write_text(json.dumps(report))
        with contextlib.redirect_stdout(io.StringIO()):
            row = DIAGNOSTIC.summarize_group(self.args, "plain", self.out, [self.observation()], {}, [])
        self.assertEqual(row["full_load_overlap_seconds"], {"1": 500/1e9})
        path = self.out / "plain-fragmented.json"
        report = json.loads(path.read_text())
        report["load_windows"][0].update(ready_unix_nano=2500, send_until_unix_nano=3500)
        path.write_text(json.dumps(report))
        with self.assertRaisesRegex(AssertionError, "did not overlap all processes"):
            DIAGNOSTIC.summarize_group(self.args, "plain", self.out, [self.observation()], {}, [])

    def test_absent_live_tcp_telemetry_is_rejected(self):
        samples = [self.observation(), self.observation({})]
        with self.assertRaisesRegex(RuntimeError, "missing TCP telemetry"):
            DIAGNOSTIC.summarize_group(
                self.args, "plain", self.out, samples, {"101": "control"}, [],
            )


if __name__ == "__main__":
    unittest.main()
