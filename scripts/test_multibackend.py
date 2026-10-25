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

    def test_bulk_downstream_waits_until_upstream_starts_after_barrier(self):
        backend, connection = self.backend()
        backend.defer_downstream = True
        backend.strict_identities = True
        connection.sendall(fixture.HELLO.pack(b"B", 10, 20, 2))
        self.assertEqual(
            fixture.receive(connection, fixture.SIZE), fixture.payload("down", "B", 10, 20, 0)
        )
        connection.settimeout(0.1)
        with self.assertRaises(socket.timeout):
            connection.recv(1)
        self.assertEqual(backend.records, 0)
        connection.settimeout(3)
        connection.sendall(
            fixture.payload("up", "B", 10, 20, 0)
            + fixture.payload("up", "B", 10, 20, 1)
            + fixture.payload("up-end", "B", 10, 20, 2)
        )
        self.assertEqual(
            fixture.receive(connection, fixture.SIZE), fixture.payload("down", "B", 10, 20, 1)
        )
        self.assertEqual(
            fixture.receive(connection, fixture.SIZE), fixture.payload("down-end", "B", 10, 20, 2)
        )
        self.assertEqual(connection.recv(1), b"")
        fixture.eventually(lambda: backend.active() == 0)
        self.assertEqual(backend.records, 2)
        self.assertFalse(backend.errors)

    def test_duplicate_backend_connection_is_detected(self):
        backend, connection = self.backend()
        backend.strict_identities = True
        connection.sendall(fixture.HELLO.pack(b"B", 11, 21, 1))
        fixture.receive(connection, fixture.SIZE)
        with socket.create_connection(backend.listener.getsockname(), 2) as duplicate:
            duplicate.sendall(fixture.HELLO.pack(b"B", 11, 21, 1))
            fixture.eventually(lambda: bool(backend.errors))
        self.assertIn("duplicate backend stream", backend.errors[0])
        connection.sendall(
            fixture.payload("up", "B", 11, 21, 0) + fixture.payload("up-end", "B", 11, 21, 1)
        )
        fixture.receive(connection, fixture.SIZE)
        self.assertEqual(connection.recv(1), b"")


if __name__ == "__main__":
    unittest.main()
