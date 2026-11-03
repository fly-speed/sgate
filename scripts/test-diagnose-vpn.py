#!/usr/bin/env python3
import importlib.util
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import json

spec = importlib.util.spec_from_file_location("diagnose_vpn", Path(__file__).with_name("diagnose-vpn.py"))
diag = importlib.util.module_from_spec(spec)
spec.loader.exec_module(diag)

class DiagnosticTest(unittest.TestCase):
    def test_report_discards_sensitive_curl_metadata(self):
        raw = {"http_code":200,"remote_ip":"192.0.2.1","time_total":1,"certs":"certificate-data",
               "url_effective":"https://example.com/?token=private","proxy_used":1}
        with patch.object(diag,"command",return_value={"stdout":json.dumps(raw),"stderr":"","exitcode":0}) as run:
            result = diag.probe("curl","mail.google.com","http1.1","192.0.2.1",8)
        self.assertTrue(result["https_response_received"])
        self.assertNotIn("private",json.dumps(result))
        self.assertNotIn("certs",result)
        args=run.call_args.args[0]
        self.assertEqual(args[:2],["curl","-q"])
        self.assertIn("--resolve",args)
        self.assertIn("--head",args)
        self.assertEqual(args[args.index("--proxy")+1],"")

    def test_partial_body_is_not_a_completed_page(self):
        raw = {"http_code":200,"size_download":12345,"time_connect":0.2,"time_appconnect":0.6}
        with patch.object(diag,"command",return_value={"stdout":json.dumps(raw),"stderr":"timeout","exitcode":28}) as run:
            result=diag.probe("curl","mail.google.com","http2",None,15,download=True)
        self.assertTrue(result["https_response_received"])
        self.assertFalse(result["transfer_completed"])
        self.assertEqual(result["stage"],"partial_body")
        self.assertNotIn("--head",run.call_args.args[0])
        self.assertIn("--location",run.call_args.args[0])

    def test_checkpoints_remain_valid_after_rewrite(self):
        with tempfile.TemporaryFile(mode="w+") as output:
            diag.checkpoint({"probes":[{"error":"long error"}]},output)
            diag.checkpoint({"probes":[]},output)
            output.seek(0)
            self.assertEqual(json.load(output),{"probes":[]})

if __name__ == "__main__":
    unittest.main()
