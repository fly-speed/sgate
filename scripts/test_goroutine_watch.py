"""Check recovery accounting without treating live 128-stream load as a leak."""

import importlib.util
import json
from pathlib import Path
import tempfile
import unittest


SPEC = importlib.util.spec_from_file_location(
    "goroutine_watch", Path(__file__).with_name("analyze-goroutine-watch.py")
)
WATCH = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(WATCH)

STACK = """goroutine 7 [running]:
main.configSoakWatch.func1.2(0x1, 0x0)
\t/diagnostic/main.go:1 +0x1

goroutine 8 [sync.Mutex.Lock]:
internal/sync.runtime_SemacquireMutex(0x1, 0x0, 0x0)
\t/runtime/sema.go:1 +0x1
sgate/internal/tunnel.(*session).readStream(0x1, 0x2, 0x3)
\t/internal/tunnel/session.go:1 +0x1
"""


class WatchRecoveryTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name)

    def sample(self, step, elapsed, goroutines, streams):
        path = self.root / f"server.cf.metrics.watch-1.event-1-{step}.json"
        path.write_text(
            json.dumps(
                dict(
                    pid=1,
                    elapsed_ms=elapsed,
                    goroutines=goroutines,
                    trigger_goroutines=44,
                    census=dict(streams=streams, pending_bytes=0),
                )
            )
        )
        path.with_suffix(".stack").write_text(STACK)
        return path

    def test_full_stack_counts_and_reader_mutex_ownership(self):
        path = self.sample(0, 0, 44, 0)
        result = WATCH.stack_summary(path.with_suffix(".stack"))
        self.assertEqual(result["count"], 2)
        self.assertEqual(result["mutex_stream_readers"], 1)
        self.assertEqual(result["stream_readers"], 1)

    def test_resumed_128_streams_and_idle_recovery_are_separate(self):
        path = self.sample(0, 0, 44, 0)
        self.sample(1, 100, 144, 128)
        self.sample(2, 500, 16, 0)
        result = WATCH.episode(path)
        self.assertEqual(result["first_normalized_recovery_ms"], 100)
        self.assertEqual(result["first_idle_recovery_ms"], 500)
        self.assertIn("closed/replaced session", result["attribution"])

    def test_process_exit_does_not_imply_observed_recovery(self):
        path = self.sample(0, 0, 44, 0)
        result = WATCH.episode(path)
        self.assertTrue(result["followup_ended_early"])
        self.assertIsNone(result["first_idle_recovery_ms"])
        self.assertIsNone(result["first_normalized_recovery_ms"])
