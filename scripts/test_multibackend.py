"""Verify that the stress fixture detects wrong targets and duplicate records."""

import importlib.util
import pathlib
import socket
import unittest

spec = importlib.util.spec_from_file_location(
    "multibackend", pathlib.Path(__file__).with_name("test-multibackend.py")
)
fixture = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fixture)


class ProtocolChecks(unittest.TestCase):
    def backend(self):
        backend = fixture.Backend("B")
        self.addCleanup(backend.close)
        connection = socket.create_connection(backend.listener.getsockname(), 2)
        connection.settimeout(3)
        self.addCleanup(connection.close)
        return backend, connection

    def test_wrong_backend_is_detected(self):
        backend, connection = self.backend()
        connection.sendall(fixture.HELLO.pack(b"A", 1, 7, 0))
        fixture.eventually(lambda: bool(backend.errors))
        self.assertIn("wrong backend", backend.errors[0])

    def test_duplicate_upstream_is_detected(self):
        backend, connection = self.backend()
        connection.sendall(fixture.HELLO.pack(b"B", 2, 9, 2))
        self.assertEqual(
            fixture.receive(connection, fixture.SIZE), fixture.payload("down", "B", 2, 9, 0)
        )
        repeated = fixture.payload("up", "B", 2, 9, 0)
        connection.sendall(repeated + repeated)
        fixture.eventually(lambda: bool(backend.errors))
        self.assertIn("upstream loss/duplicate/cross-stream", backend.errors[0])
        self.assertEqual(backend.records, 1)

    def test_held_connection_can_close_without_a_false_data_failure(self):
        backend, connection = self.backend()
        connection.sendall(fixture.HELLO.pack(b"B", 3, 11, 0))
        self.assertEqual(
            fixture.receive(connection, fixture.SIZE), fixture.payload("down", "B", 3, 11, 0)
        )
        connection.close()
        fixture.eventually(lambda: backend.active() == 0)
        self.assertFalse(backend.errors)


if __name__ == "__main__":
    unittest.main()
