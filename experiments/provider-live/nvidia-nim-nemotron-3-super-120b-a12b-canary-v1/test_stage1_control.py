import io
import json
import os
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from urllib.error import HTTPError

import stage1_control as control


class _Response:
    def __init__(self, status, body):
        self.status = status
        self.body = body

    def read(self, limit):
        return self.body[:limit]

    def __enter__(self):
        return self

    def __exit__(self, *_args):
        return False


class _Opener:
    def __init__(self, response=None, error=None):
        self.response = response
        self.error = error
        self.calls = []

    def open(self, request, timeout):
        self.calls.append((request, timeout))
        if self.error:
            raise self.error
        return self.response


class Stage1ControlTests(unittest.TestCase):
    def test_fixed_request_and_response_shape_and_model_are_separate(self):
        payload = {
            "object": "chat.completion",
            "model": control.MODEL_ID,
            "choices": [{"message": {"role": "assistant", "content": "O"}}],
        }
        opener = _Opener(_Response(200, json.dumps(payload).encode()))
        result = control.dispatch_once(
            "SYNTHETIC_KEY_SENTINEL", opener_factory=lambda *_args: opener,
        )

        self.assertEqual(result.observation, {
            "request_count": 1,
            "method": "POST",
            "path": "/chat/completions",
            "http_status": 200,
            "error_type": "none",
        })
        self.assertTrue(result.response_shape_valid)
        self.assertTrue(result.returned_model_matches)
        self.assertEqual(len(opener.calls), 1)
        request, timeout = opener.calls[0]
        self.assertEqual(timeout, control.TIMEOUT_SECONDS)
        self.assertEqual(request.full_url, "https://integrate.api.nvidia.com/v1/chat/completions")
        self.assertEqual(request.get_method(), "POST")
        self.assertEqual(request.get_header("Authorization"), "Bearer SYNTHETIC_KEY_SENTINEL")
        self.assertEqual(json.loads(request.data), {
            "model": "nvidia/nemotron-3-super-120b-a12b",
            "messages": [{"role": "user", "content": "Reply with OK."}],
            "max_tokens": 1,
            "stream": False,
        })

    def test_model_mismatch_does_not_change_valid_shape_result(self):
        payload = {
            "object": "chat.completion",
            "model": "another/model",
            "choices": [{"message": {"role": "assistant", "content": "O"}}],
        }
        opener = _Opener(_Response(200, json.dumps(payload).encode()))
        result = control.dispatch_once(
            "SYNTHETIC_KEY_SENTINEL", opener_factory=lambda *_args: opener,
        )
        self.assertTrue(result.response_shape_valid)
        self.assertFalse(result.returned_model_matches)
        self.assertEqual(result.request_count, 1)

    def test_redirect_is_refused_without_second_request(self):
        paths = []

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):
                paths.append(self.path)
                self.rfile.read(int(self.headers["Content-Length"]))
                self.send_response(302)
                self.send_header("Location", "/second-request")
                self.end_headers()

            def log_message(self, *_args):
                pass

        server = HTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            result = control._dispatch_once(
                f"http://127.0.0.1:{server.server_port}/v1", control.MODEL_ID,
                "SYNTHETIC_KEY_SENTINEL",
            )
        finally:
            server.shutdown()
            thread.join()
            server.server_close()

        self.assertEqual(paths, ["/v1/chat/completions"])
        self.assertEqual(result.observation["http_status"], 302)
        self.assertFalse(result.response_shape_valid)
        self.assertFalse(result.returned_model_matches)
        self.assertEqual(result.request_count, 1)

    def test_provider_error_body_headers_and_exception_text_are_not_rendered(self):
        error = HTTPError(
            "https://invalid.example/chat/completions?token=QUERY_SECRET_SENTINEL",
            401, "EXCEPTION_SECRET_SENTINEL",
            {"X-Private": "HEADER_SECRET_SENTINEL"},
            io.BytesIO(b"BODY_SECRET_SENTINEL"),
        )
        opener = _Opener(error=error)
        result = control.dispatch_once(
            "SYNTHETIC_KEY_SENTINEL", opener_factory=lambda *_args: opener,
        )
        rendered = control.render_result(result)
        self.assertEqual(result.observation["http_status"], 401)
        self.assertEqual(result.request_count, 1)
        self.assertFalse(result.response_shape_valid)
        self.assertFalse(result.returned_model_matches)
        self.assertEqual(len(opener.calls), 1)
        for forbidden in (
            "QUERY_SECRET_SENTINEL", "EXCEPTION_SECRET_SENTINEL",
            "HEADER_SECRET_SENTINEL", "BODY_SECRET_SENTINEL",
            "SYNTHETIC_KEY_SENTINEL",
        ):
            self.assertNotIn(forbidden, rendered)

    def test_env_loader_uses_only_explicit_file_and_emits_boolean_checks(self):
        with tempfile.TemporaryDirectory() as directory:
            env_file = Path(directory) / "nvidia.env"
            env_file.write_text("NVIDIA_API_KEY='FILE_KEY_SENTINEL'\n", encoding="utf-8")
            os.environ["NVIDIA_API_KEY"] = "AMBIENT_KEY_SENTINEL"
            checks, key = control.load_key_without_emitting(env_file)
            loaded_value = os.environ.get("NVIDIA_API_KEY")
            os.environ.pop("NVIDIA_API_KEY", None)

        self.assertEqual(key, "FILE_KEY_SENTINEL")
        self.assertEqual(loaded_value, "FILE_KEY_SENTINEL")
        self.assertEqual(checks, {
            "env_file_regular": True,
            "nvidia_key_declaration_present": True,
            "nvidia_key_loaded": True,
            "nvidia_key_nonempty": True,
            "secret_value_emitted": False,
        })


if __name__ == "__main__":
    unittest.main()
