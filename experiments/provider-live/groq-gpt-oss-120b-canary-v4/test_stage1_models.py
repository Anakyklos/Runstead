import io
import json
import unittest
from unittest.mock import patch
from urllib.request import Request

import stage1_models


class FakeResponse(io.BytesIO):
    status = 200

    def __enter__(self):
        return self

    def __exit__(self, *_args):
        self.close()


class FakeOpener:
    def __init__(self, payload):
        self.payload = payload
        self.calls = []

    def open(self, request, timeout):
        self.calls.append((request, timeout))
        return FakeResponse(json.dumps(self.payload).encode())


class Stage1ModelsTests(unittest.TestCase):
    def test_exact_model_is_checked_with_one_authenticated_get(self):
        opener = FakeOpener({"data": [{"id": "openai/gpt-oss-120b"}, {"id": "other"}]})
        status, present = stage1_models.request_model_presence("secret-for-test", lambda *_: opener)

        self.assertEqual((status, present), (200, True))
        self.assertEqual(len(opener.calls), 1)
        request, timeout = opener.calls[0]
        self.assertEqual(request.method, "GET")
        self.assertEqual(request.full_url, "https://api.groq.com/openai/v1/models")
        self.assertEqual(request.get_header("User-agent"), stage1_models.USER_AGENT)
        self.assertEqual(request.get_header("Authorization"), "Bearer secret-for-test")
        self.assertEqual(timeout, 30)

    def test_missing_exact_model_does_not_select_an_alternate(self):
        opener = FakeOpener({"data": [{"id": "other"}]})
        status, present = stage1_models.request_model_presence("secret-for-test", lambda *_: opener)

        self.assertEqual((status, present), (200, False))
        self.assertEqual(len(opener.calls), 1)


if __name__ == "__main__":
    unittest.main()
