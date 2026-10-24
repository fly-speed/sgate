import importlib.util
import pathlib
import socket
import unittest

spec = importlib.util.spec_from_file_location(
    "retention", pathlib.Path(__file__).with_name("test-send-addr-retention.py")
)
retention = importlib.util.module_from_spec(spec)
spec.loader.exec_module(retention)
multi = retention.multi


class BackendTests(unittest.TestCase):
    def setUp(self):
        self.backend = retention.Backend("A")
        self.addCleanup(self.backend.close)
        self.connection = socket.create_connection(tuple(self.backend.address.split(":")))
        self.connection.settimeout(2)
        self.addCleanup(self.connection.close)

    def test_gate_and_unexpected_upstream(self):
        self.backend.gate.clear()
        self.connection.sendall(multi.HELLO.pack(b"A", 1, 1, 0))
        self.assertEqual(
            multi.receive(self.connection, multi.SIZE), multi.payload("down", "A", 1, 1, 0)
        )
        self.connection.settimeout(0.02)
        with self.assertRaises(socket.timeout):
            self.connection.recv(1)
        self.backend.gate.set()
        self.connection.settimeout(2)
        self.assertEqual(
            multi.receive(self.connection, multi.SIZE * 16),
            multi.payload("discard", "A", 1, 1, 0) * 16,
        )
        self.connection.sendall(b"unexpected")
        multi.eventually(lambda: self.backend.errors)

    def test_strict_duplex(self):
        self.connection.sendall(multi.HELLO.pack(b"A", 2, 3, 2))
        self.connection.sendall(
            multi.payload("up", "A", 2, 3, 0)
            + multi.payload("up", "A", 2, 3, 1)
            + multi.payload("up-end", "A", 2, 3, 2)
        )
        self.assertEqual(
            multi.receive(self.connection, multi.SIZE * 3),
            multi.payload("down", "A", 2, 3, 0)
            + multi.payload("down", "A", 2, 3, 1)
            + multi.payload("down-end", "A", 2, 3, 2),
        )
        self.assertEqual(self.connection.recv(1), b"")
        self.assertFalse(self.backend.errors)

    def test_duplicate_record_rejected(self):
        self.connection.sendall(multi.HELLO.pack(b"A", 2, 3, 2))
        self.connection.sendall(multi.payload("up", "A", 2, 3, 0) * 2)
        multi.eventually(lambda: self.backend.errors)
        self.assertIn("upstream integrity", str(self.backend.errors))


if __name__ == "__main__":
    unittest.main()
