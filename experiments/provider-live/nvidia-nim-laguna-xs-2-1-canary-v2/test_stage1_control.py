import io
import json
import os
import tempfile
import unittest
from pathlib import Path
from urllib.error import HTTPError, URLError

import stage1_control as control


class Response:
    def __init__(self, status, body):
        self.status = status
        self.body = body

    def read(self, limit):
        return self.body[:limit]

    def __enter__(self):
        return self

    def __exit__(self, *_):
        return False


class Opener:
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
    def test_explicit_env_file_is_source_and_inherited_value_is_ignored(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "nvidia.env"
            path.write_text("NVIDIA_API_KEY='FILE_KEY_SENTINEL'\n", encoding="utf-8")
            os.environ["NVIDIA_API_KEY"] = "AMBIENT_SENTINEL"
            key, checks = control.load_key_from_explicit_file(path, inherited_key="AMBIENT_SENTINEL")
            loaded_env_key = os.environ.get("NVIDIA_API_KEY")
            os.environ.pop("NVIDIA_API_KEY", None)
        self.assertEqual(key, "FILE_KEY_SENTINEL")
        self.assertEqual(loaded_env_key, "FILE_KEY_SENTINEL")
        self.assertTrue(checks["env_file_regular"])
        self.assertTrue(checks["nvidia_key_declaration_present"])
        self.assertTrue(checks["nvidia_key_loaded"])
        self.assertTrue(checks["nvidia_key_nonempty"])
        self.assertFalse(checks["secret_value_emitted"])

    def test_control_sends_exact_single_chat_completion_request_and_accepts_structure(self):
        opener = Opener(Response(200, json.dumps({
            "model": control.EXPECTED_MODEL,
            "choices": [{"message": {"role": "assistant", "content": "O"}}],
        }).encode()))
        observation, valid = control.request_exact_model("KEY_SENTINEL", opener_factory=lambda *_: opener)
        self.assertEqual(observation, {
            "request_count": 1,
            "method": "POST",
            "path": "/chat/completions",
            "http_status": 200,
            "error_type": "none",
        })
        self.assertTrue(valid)
        self.assertEqual(len(opener.calls), 1)
        request, timeout = opener.calls[0]
        self.assertEqual(timeout, 60)
        self.assertEqual(request.full_url, control.BASE_URL + "/chat/completions")
        self.assertEqual(request.get_method(), "POST")
        self.assertEqual(json.loads(request.data), {
            "model": "poolside/laguna-xs-2.1",
            "messages": [{"role": "user", "content": "Reply with OK."}],
            "max_tokens": 1,
            "stream": False,
        })
        self.assertEqual(request.get_header("Authorization"), "Bearer KEY_SENTINEL")

    def test_http_error_keeps_only_helper_allowlisted_fields_and_numeric_status(self):
        opener = Opener(error=HTTPError(
            "https://invalid.example/chat/completions?secret=DO_NOT_EMIT",
            401,
            "PRIVATE_EXCEPTION_SENTINEL",
            {"X-Private": "PRIVATE_HEADER_SENTINEL"},
            io.BytesIO(b"PRIVATE_BODY_SENTINEL"),
        ))
        observation, valid = control.request_exact_model("KEY_SENTINEL", opener_factory=lambda *_: opener)
        self.assertFalse(valid)
        self.assertEqual(observation, {
            "request_count": 1,
            "method": "POST",
            "path": "/chat/completions",
            "http_status": 401,
            "error_type": "HTTPError",
        })
        self.assertEqual(len(opener.calls), 1)
        rendered = control.render_observation(observation)
        self.assertEqual(rendered.splitlines(), [
            "request_count=1", "method=POST", "path=/chat/completions",
            "http_status=401", "error_type=HTTPError",
        ])
        for forbidden in ("DO_NOT_EMIT", "PRIVATE_EXCEPTION_SENTINEL", "PRIVATE_HEADER_SENTINEL", "PRIVATE_BODY_SENTINEL"):
            self.assertNotIn(forbidden, rendered)

    def test_wrong_returned_model_fails_separate_exact_model_validation(self):
        opener = Opener(Response(200, b'{"model":"other/model","choices":[{"message":{}}]}'))
        observation, valid = control.request_exact_model("KEY_SENTINEL", opener_factory=lambda *_: opener)
        self.assertEqual(observation["http_status"], 200)
        self.assertFalse(valid)

    def test_transport_error_is_sanitized_and_does_not_retry(self):
        opener = Opener(error=URLError("PRIVATE_TRANSPORT_SENTINEL"))
        observation, valid = control.request_exact_model("KEY_SENTINEL", opener_factory=lambda *_: opener)
        self.assertFalse(valid)
        self.assertEqual(observation["http_status"], "unknown")
        self.assertEqual(observation["error_type"], "URLError")
        self.assertEqual(len(opener.calls), 1)


if __name__ == "__main__":
    unittest.main()
