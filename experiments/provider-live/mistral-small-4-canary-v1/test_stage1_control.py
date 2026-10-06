import json
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer

import stage1_control


class Stage1ControlTests(unittest.TestCase):
    def serve(self, status, payload):
        captured = {}

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):
                captured["path"] = self.path
                captured["authorization"] = self.headers.get("Authorization")
                captured["body"] = self.rfile.read(int(self.headers["Content-Length"]))
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(payload)

            def log_message(self, *_args):
                pass

        server = HTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        return server, thread, captured

    def test_exact_request_and_separate_response_validation(self):
        payload = json.dumps({
            "id": "synthetic",
            "object": "chat.completion",
            "created": 1,
            "model": "mistral-small-2603",
            "choices": [{"index": 0, "message": {"role": "assistant", "content": "OK"}, "finish_reason": "stop"}],
        }).encode()
        server, thread, captured = self.serve(200, payload)
        try:
            result = stage1_control.dispatch_once(
                f"http://127.0.0.1:{server.server_port}/v1",
                "mistral-small-2603",
                "synthetic-test-key",
            )
        finally:
            server.shutdown()
            thread.join()
            server.server_close()
        self.assertEqual(captured["path"], "/v1/chat/completions")
        self.assertEqual(captured["authorization"], "Bearer synthetic-test-key")
        self.assertEqual(json.loads(captured["body"]), {
            "model": "mistral-small-2603",
            "messages": [{"role": "user", "content": "Reply with OK."}],
            "max_tokens": 1,
            "stream": False,
        })
        self.assertEqual(result.observation, {
            "request_count": 1,
            "method": "POST",
            "path": "/v1/chat/completions",
            "http_status": 200,
            "error_type": "none",
        })
        self.assertTrue(result.response_shape_valid)
        self.assertTrue(result.returned_model_matches)

    def test_http_error_preserves_status_without_body_or_exception_text(self):
        server, thread, _captured = self.serve(401, b"secret-looking provider error body")
        try:
            result = stage1_control.dispatch_once(
                f"http://127.0.0.1:{server.server_port}/v1",
                "mistral-small-2603",
                "synthetic-test-key",
            )
        finally:
            server.shutdown()
            thread.join()
            server.server_close()
        self.assertEqual(result.observation["http_status"], 401)
        self.assertEqual(result.observation["error_type"], "HTTPError")
        self.assertEqual(result.observation["request_count"], 1)
        rendered = stage1_control.render_result(result)
        self.assertNotIn("secret-looking", rendered)
        self.assertNotIn("synthetic-test-key", rendered)

    def test_redirect_is_not_followed(self):
        captured_paths = []

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):
                captured_paths.append(self.path)
                self.rfile.read(int(self.headers["Content-Length"]))
                self.send_response(302)
                self.send_header("Location", "/redirect-target")
                self.end_headers()

            def log_message(self, *_args):
                pass

        server = HTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            result = stage1_control.dispatch_once(
                f"http://127.0.0.1:{server.server_port}/v1",
                "mistral-small-2603",
                "synthetic-test-key",
            )
        finally:
            server.shutdown()
            thread.join()
            server.server_close()
        self.assertEqual(captured_paths, ["/v1/chat/completions"])
        self.assertEqual(result.observation["http_status"], 302)
        self.assertEqual(result.request_count, 1)

    def test_redirect_is_not_followed_and_model_mismatch_fails_validation(self):
        payload = json.dumps({
            "id": "synthetic",
            "object": "chat.completion",
            "created": 1,
            "model": "mistral-small-2603-2026-03",
            "choices": [{"index": 0, "message": {"role": "assistant", "content": "OK"}, "finish_reason": "stop"}],
        }).encode()
        server, thread, captured = self.serve(200, payload)
        try:
            result = stage1_control.dispatch_once(
                f"http://127.0.0.1:{server.server_port}/v1",
                "mistral-small-2603",
                "synthetic-test-key",
            )
        finally:
            server.shutdown()
            thread.join()
            server.server_close()
        self.assertFalse(result.returned_model_matches)
        self.assertEqual(result.request_count, 1)


if __name__ == "__main__":
    unittest.main()
