import io
import unittest
from urllib.error import HTTPError, URLError

from sanitized_http import (
    http_error_observation,
    render_observation,
    response_observation,
    transport_error_observation,
)


class SanitizedHTTPTests(unittest.TestCase):
    def test_successful_http_200_keeps_only_allowlisted_request_metadata(self):
        observation = response_observation(1, "GET", "/models", 200)

        self.assertEqual(
            observation,
            {
                "request_count": 1,
                "method": "GET",
                "path": "/models",
                "http_status": 200,
                "error_type": "none",
            },
        )
        self.assertEqual(set(observation), {
            "request_count", "method", "path", "http_status", "error_type"
        })

    def test_http_errors_preserve_numeric_status_without_emitting_exception_data(self):
        for status in (401, 403, 429, 503):
            with self.subTest(status=status):
                exception = HTTPError(
                    "https://provider.invalid/v1/models?token=QUERY_SECRET_SENTINEL",
                    status,
                    "EXCEPTION_TEXT_SENTINEL",
                    {
                        "X-Arbitrary": "ARBITRARY_HEADER_SENTINEL",
                        "Authorization": "Bearer API_KEY_SENTINEL",
                        "X-Key-Hash": "API_KEY_HASH_SENTINEL",
                        "X-Key-Prefix": "API_KEY_PREFIX_SENTINEL",
                        "X-Key-Suffix": "API_KEY_SUFFIX_SENTINEL",
                        "X-Key-Length": "API_KEY_LENGTH_SENTINEL",
                    },
                    io.BytesIO(
                        b"PRIVATE_PROMPT_SENTINEL PRIVATE_RESPONSE_SENTINEL "
                        b"PRIVATE_RESPONSE_BODY_SENTINEL"
                    ),
                )
                rendered = render_observation(
                    http_error_observation(1, "POST", "/v1/chat/completions", exception)
                )

                self.assertEqual(
                    rendered.splitlines(),
                    [
                        "request_count=1",
                        "method=POST",
                        "path=/v1/chat/completions",
                        f"http_status={status}",
                        "error_type=HTTPError",
                    ],
                )
                for forbidden in (
                    "PRIVATE_RESPONSE_BODY_SENTINEL",
                    "ARBITRARY_HEADER_SENTINEL",
                    "EXCEPTION_TEXT_SENTINEL",
                    "Authorization",
                    "API_KEY_SENTINEL",
                    "API_KEY_HASH_SENTINEL",
                    "API_KEY_PREFIX_SENTINEL",
                    "API_KEY_SUFFIX_SENTINEL",
                    "API_KEY_LENGTH_SENTINEL",
                    "QUERY_SECRET_SENTINEL",
                    "PRIVATE_PROMPT_SENTINEL",
                    "PRIVATE_RESPONSE_SENTINEL",
                ):
                    self.assertNotIn(forbidden, rendered)

    def test_transport_failure_keeps_status_explicitly_unknown(self):
        exception = URLError("TRANSPORT_EXCEPTION_TEXT_SENTINEL")
        rendered = render_observation(
            transport_error_observation(1, "GET", "/models", exception)
        )

        self.assertIn("request_count=1", rendered)
        self.assertIn("method=GET", rendered)
        self.assertIn("path=/models", rendered)
        self.assertIn("http_status=unknown", rendered)
        self.assertIn("error_type=URLError", rendered)
        self.assertNotIn("TRANSPORT_EXCEPTION_TEXT_SENTINEL", rendered)

    def test_query_strings_are_rejected_from_request_path(self):
        with self.assertRaises(ValueError):
            response_observation(1, "GET", "/models?token=QUERY_SECRET_SENTINEL", 200)


if __name__ == "__main__":
    unittest.main()
