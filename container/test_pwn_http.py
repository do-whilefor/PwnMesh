import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import socket
import stat
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


spec = importlib.util.spec_from_file_location("pwn_http", Path(__file__).with_name("pwn-http.py"))
http = importlib.util.module_from_spec(spec)
spec.loader.exec_module(http)


class CaptureTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.received = []

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def do_GET(self):
                body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
                cls.received.append((self.command, self.path, self.headers, body))
                if self.path.startswith("/redirect"):
                    self.send_response(302)
                    self.send_header("Location", "/destination")
                elif self.path.startswith("/denied"):
                    self.send_response(403)
                else:
                    self.send_response(200)
                self.send_header("Content-Length", "20" if self.path == "/short" else "4")
                self.send_header("Set-Cookie", "session=response-secret; HttpOnly")
                self.send_header("Connection", "close")
                self.end_headers()
                self.wfile.write(b"done")
                self.close_connection = True

            do_POST = do_GET

            def log_message(self, *args):
                pass

        cls.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()
        cls.origin = "http://127.0.0.1:" + str(cls.server.server_port)

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()
        cls.thread.join()

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.received.clear()

    def entry(self, **kwargs):
        request = {"method": "GET", "url": self.origin + "/path?token=query-secret&n=1", "headers": []}
        request.update(kwargs)
        return {"request": request}

    def capture(self, *entries):
        path = self.root / "capture.har"
        path.write_text(json.dumps({"log": {"entries": list(entries)}}), encoding="utf-8")
        return str(path)

    def call(self, *args):
        out, err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            code = http.main(list(args))
        return code, out.getvalue(), err.getvalue()

    def test_inspection_never_sends_or_displays_authentication(self):
        path = self.capture(self.entry(headers=[{"name": "Authorization", "value": "Bearer header-secret"},
                                               {"name": "Cookie", "value": "s=cookie-secret"}]))
        code, out, err = self.call("inspect", path)
        self.assertEqual((code, err), (0, ""))
        self.assertFalse(self.received)
        self.assertNotIn("secret", out)
        self.assertTrue(json.loads(out)[0]["replayable"])
        self.assertEqual(json.loads(out)[0]["index"], 1)

    def test_exact_raw_binary_body_and_query_replayed_once(self):
        body = b"\x00\xff\xfe\r\nbody\r\n"
        path = self.root / "request.http"
        target = "/a%2fb?x=1&x=2&q=%E4%B8%AD&blank="
        path.write_bytes(("POST " + target + " HTTP/1.1\r\nHost: 127.0.0.1:" + str(self.server.server_port) +
                          "\r\nContent-Type: application/octet-stream\r\nContent-Length: " + str(len(body)) + "\r\n\r\n").encode() + body)
        output = self.root / "evidence"
        code, out, err = self.call("replay", str(path), "--base-url", self.origin, "--index", "1", "--output", str(output))
        self.assertEqual((code, err), (0, ""))
        self.assertEqual(len(self.received), 1)
        method, received_target, headers, received_body = self.received[0]
        self.assertEqual((method, received_target, received_body), ("POST", target, body))
        self.assertEqual(headers["Content-Type"], "application/octet-stream")
        self.assertEqual((output / "request.body").read_bytes(), body)
        self.assertEqual((output / "response.body").read_bytes(), b"done")
        response = json.loads((output / "response.json").read_text())
        self.assertTrue(response["complete"])
        self.assertNotIn("response-secret", json.dumps(response))
        self.assertEqual(json.loads(out)["status"], 200)
        if os.name == "posix":
            self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o700)
            for child in output.iterdir():
                self.assertEqual(stat.S_IMODE(child.stat().st_mode), 0o600)

    def test_selected_entry_cookie_fallback_and_role_overrides(self):
        path = self.capture(self.entry(url=self.origin + "/unused"), self.entry(
            headers=[{"name": "Authorization", "value": "Bearer account-a"}, {"name": "X-Remove", "value": "remove"}],
            cookies=[{"name": "sid", "value": "account-a"}]))
        headers_path = self.root / "role-b.json"
        headers_path.write_text(json.dumps({"authorization": "Bearer account-b", "Cookie": "sid=account-b", "X-Remove": None}))
        code, _, err = self.call("replay", path, "--index", "2", "--output", str(self.root / "role-b"), "--headers", str(headers_path))
        self.assertEqual((code, err), (0, ""))
        self.assertEqual(len(self.received), 1)
        self.assertEqual(self.received[0][2]["Authorization"], "Bearer account-b")
        self.assertEqual(self.received[0][2]["Cookie"], "sid=account-b")
        self.assertIsNone(self.received[0][2]["X-Remove"])
        code, _, err = self.call("replay", path, "--index", "2", "--output", str(self.root / "anonymous"), "--remove-header", "cookie", "--remove-header", "AUTHORIZATION")
        self.assertEqual((code, err), (0, ""))
        self.assertIsNone(self.received[1][2]["Authorization"])
        self.assertIsNone(self.received[1][2]["Cookie"])
        parsed = http.har_request(self.entry(cookies=[{"name": "sid", "value": "account-a"}]))
        self.assertEqual(http.values(parsed["headers"], "Cookie"), ["sid=account-a"])

    def test_original_cookie_header_wins_over_har_cookie_list(self):
        request = http.har_request(self.entry(headers=[{"name": "Cookie", "value": "sid=original"}], cookies=[{"name": "sid", "value": "different"}]))
        self.assertEqual(http.values(request["headers"], "Cookie"), ["sid=original"])

    def test_utf8_and_base64_har_bodies(self):
        cases = [
            ({"mimeType": "application/json; charset=utf-8", "text": '{"name":"中文"}'}, '{"name":"中文"}'.encode()),
            ({"mimeType": "application/octet-stream", "text": "AP8=", "encoding": "base64"}, b"\x00\xff"),
            ({"mimeType": "multipart/form-data; boundary=x", "text": "LS14LS0NCg==", "_encoding": "base64"}, b"--x--\r\n"),
        ]
        for n, (post, body) in enumerate(cases):
            with self.subTest(post=post):
                path = self.capture(self.entry(method="POST", postData=post, bodySize=len(body)))
                code, _, err = self.call("replay", path, "--index", "1", "--output", str(self.root / str(n)))
                self.assertEqual((code, err), (0, ""))
                self.assertEqual(self.received[-1][3], body)
                self.assertEqual(int(self.received[-1][2]["Content-Length"]), len(body))
                self.assertEqual(self.received[-1][2]["Content-Type"], post["mimeType"])

    def test_redirects_not_followed_and_error_responses_saved(self):
        for route, status in [("/redirect", 302), ("/denied", 403)]:
            path = self.capture(self.entry(url=self.origin + route))
            output = self.root / route[1:]
            code, out, err = self.call("replay", path, "--index", "1", "--output", str(output))
            self.assertEqual((code, err), (0, ""))
            self.assertEqual(json.loads(out)["status"], status)
            self.assertEqual((output / "response.body").read_bytes(), b"done")
        self.assertEqual([request[1] for request in self.received], ["/redirect", "/denied"])

    def test_truncated_response_is_failure_with_partial_evidence(self):
        path = self.capture(self.entry(url=self.origin + "/short"))
        output = self.root / "partial"
        code, _, err = self.call("replay", path, "--index", "1", "--output", str(output))
        self.assertEqual(code, 1)
        self.assertIn("partial evidence", err)
        self.assertEqual((output / "response.body").read_bytes(), b"done")
        self.assertFalse(json.loads((output / "response.json").read_text())["complete"])
        self.assertTrue((output / "error.json").exists())

    def test_invalid_capture_and_selection_send_nothing(self):
        malformed = self.root / "broken.har"
        malformed.write_text("{secret invalid json")
        code, _, err = self.call("inspect", str(malformed))
        self.assertEqual(code, 1)
        self.assertNotIn("secret", err)
        malformed.write_text("[]")
        self.assertEqual(self.call("inspect", str(malformed))[0], 1)
        path = self.capture(self.entry())
        for index in ("0", "2", "-1"):
            self.assertEqual(self.call("replay", path, "--index", index, "--output", str(self.root / "never"))[0], 1)
        self.assertFalse(self.received)
        self.assertFalse((self.root / "never").exists())

    def test_invalid_body_or_headers_are_explicitly_unreplayable(self):
        invalid = [
            {"postData": {"params": [{"name": "a", "value": "b"}]}},
            {"postData": {"mimeType": "multipart/form-data", "text": "lost-binary"}},
            {"postData": {"mimeType": "application/octet-stream", "text": "lost-binary"}},
            {"postData": {"text": "中文"}},
            {"postData": {"text": "!", "encoding": "base64"}},
            {"method": "POST"},
            {"postData": {"text": "decoded"}, "headers": [{"name": "Content-Encoding", "value": "gzip"}]},
            {"bodySize": 3},
            {"headers": [{"name": "Content-Length", "value": "12"}]},
            {"headers": [{"name": "Content-Length", "value": "²"}]},
            {"headers": [{"name": "Content-Length", "value": "9" * 5000}]},
            {"headers": [{"name": "Transfer-Encoding", "value": "chunked"}]},
            {"headers": [{"name": ":authority", "value": "example.test"}]},
            {"headers": [{"name": "Cookie", "value": "x\r\nInjected: y"}]},
            {"headers": [{"name": "Host", "value": "different.test"}]},
            {"url": "https://user:password@example.test/"},
            {"url": "https://example.test/#fragment"},
            {"cookies": [{"name": "sid", "value": "bad; other=identity"}]},
        ]
        for entry in invalid:
            with self.subTest(entry=entry):
                path = self.capture(self.entry(**entry))
                code, out, err = self.call("inspect", path)
                self.assertEqual((code, err), (0, ""))
                self.assertFalse(json.loads(out)[0]["replayable"])
                self.assertEqual(self.call("replay", path, "--index", "1", "--output", str(self.root / "never"))[0], 1)
        self.assertFalse(self.received)

    def test_raw_requires_scheme_and_rejects_invalid_wire_formats(self):
        with self.assertRaisesRegex(http.CaptureError, "base-url"):
            http.raw_request(b"GET / HTTP/1.1\r\nHost: example.test\r\n\r\n", None)
        for raw in (b"PRI * HTTP/2.0\r\n\r\n", b"GET / HTTP/1.1\r\n folded: value\r\n\r\n", b"GET / HTTP/1.1\r\n", b"GET / HTTP/1.1\r\nX-A: a\vInjected: b\r\n\r\n"):
            with self.subTest(raw=raw), self.assertRaises(http.CaptureError):
                http.raw_request(raw, self.origin)
        request = http.raw_request(("GET " + self.origin + "/ HTTP/1.1\r\n\r\n").encode(), None)
        self.assertEqual(request["url"], self.origin + "/")

    def test_lf_header_separator_precedes_crlf_bytes_in_binary_body(self):
        body = b"\x00\r\n\r\n\xff"
        raw = b"POST /upload HTTP/1.1\nContent-Length: 6\n\n" + body
        request = http.raw_request(raw, self.origin)
        self.assertEqual(request["body"], body)

    def test_output_exists_and_invalid_overrides_fail_before_network(self):
        path = self.capture(self.entry())
        self.assertEqual(self.call("replay", path, "--index", "1", "--output", str(self.root))[0], 1)
        for data in ({"Cookie": "a\nInjected: b"}, {"Cookie": "a", "cookie": "b"}, {"Content-Length": "1"}, {"Host": "evil.test"}, []):
            override = self.root / "headers.json"
            override.write_text(json.dumps(data))
            self.assertEqual(self.call("replay", path, "--index", "1", "--output", str(self.root / "never"), "--headers", str(override))[0], 1)
        self.assertFalse(self.received)

    def test_timeout_values_rejected_before_sending(self):
        path = self.capture(self.entry())
        for timeout in ("0", "-1", "nan", "inf", "301"):
            self.assertEqual(self.call("replay", path, "--index", "1", "--output", str(self.root / "never"), "--timeout", timeout)[0], 1)
        self.assertFalse(self.received)
        self.assertFalse((self.root / "never").exists())

    def test_connection_failure_retains_request_and_sanitized_error(self):
        with socket.socket() as reserved:
            reserved.bind(("127.0.0.1", 0))
            path = self.capture(self.entry(url="http://127.0.0.1:" + str(reserved.getsockname()[1]) + "/?token=do-not-log"))
            output = self.root / "failed"
            code, out, err = self.call("replay", path, "--index", "1", "--output", str(output), "--timeout", "1")
        self.assertEqual(code, 1)
        self.assertNotIn("do-not-log", out + err)
        self.assertTrue((output / "request.json").exists())
        self.assertTrue((output / "error.json").exists())
        self.assertFalse((output / "response.json").exists())


if __name__ == "__main__":
    unittest.main()
