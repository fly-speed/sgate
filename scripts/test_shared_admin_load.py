"""Regression for plain-text worker shutdown versus authentication failures."""

import importlib.util
import io
from pathlib import Path
import unittest

from shared_admin_load import planned_shutdown_response


SPEC = importlib.util.spec_from_file_location(
    "acl_master_runner", Path(__file__).with_name("test-acl-master.py")
)
RUNNER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(RUNNER)


class ManagementResponseTests(unittest.TestCase):
    def response_error(self, code, body):
        class Opener:
            def open(self, request, timeout):
                response = io.BytesIO(body)
                response.code = code
                return response

        client = RUNNER.Admin("https://127.0.0.1/admin", None)
        client.opener = Opener()
        with self.assertRaises(AssertionError) as caught:
            client.call("status")
        self.assertEqual(caught.exception.args[0][1], code)
        return caught.exception

    def test_plain_text_503_requires_planned_restart(self):
        error = self.response_error(503, b"service stopping\n")
        self.assertTrue(planned_shutdown_response(error, True))
        self.assertFalse(planned_shutdown_response(error, False))

    def test_restart_cannot_hide_authentication_or_invalid_success(self):
        for code, body in [
            (401, b'{"error":"unauthorized"}'),
            (403, b'{"error":"forbidden"}'),
            (200, b"not JSON"),
            (200, b""),
            (500, b"internal error"),
        ]:
            with self.subTest(code=code, body=body):
                error = self.response_error(code, body)
                self.assertFalse(planned_shutdown_response(error, True))
