"""Allowlisted, provider-neutral observations for direct canary HTTP controls.

This module never serializes an exception. Callers provide a standard urllib
exception only to select a fixed error type and, for HTTPError, its numeric
``code``. Only the five fields in ``_FIELDS`` can be rendered.
"""

from __future__ import annotations

import re
from urllib.error import HTTPError, URLError

_FIELDS = ("request_count", "method", "path", "http_status", "error_type")
_METHOD = re.compile(r"[A-Z]{1,12}\Z")
_PATH = re.compile(r"/[A-Za-z0-9._~!$&'()*+,;=:@/-]*\Z")
_ERROR_TYPES = {"none", "HTTPError", "URLError", "OSError", "TimeoutError"}


def _request_metadata(request_count: int, method: str, path: str) -> dict[str, int | str]:
    if type(request_count) is not int or request_count < 1:
        raise ValueError("request_count must be a positive integer")
    if not isinstance(method, str) or not _METHOD.fullmatch(method):
        raise ValueError("method must be an uppercase HTTP token")
    if not isinstance(path, str) or not _PATH.fullmatch(path):
        raise ValueError("path must be an origin-form path without query or fragment")
    return {"request_count": request_count, "method": method, "path": path}


def _observation(
    request_count: int,
    method: str,
    path: str,
    http_status: int | str,
    error_type: str,
) -> dict[str, int | str]:
    record = _request_metadata(request_count, method, path)
    if http_status != "unknown" and (
        type(http_status) is not int or not 100 <= http_status <= 599
    ):
        raise ValueError("http_status must be an HTTP status or unknown")
    if error_type not in _ERROR_TYPES:
        raise ValueError("error_type is not allowlisted")
    record["http_status"] = http_status
    record["error_type"] = error_type
    return record


def response_observation(
    request_count: int, method: str, path: str, status: int
) -> dict[str, int | str]:
    """Build a successful-response observation without retaining response data."""
    if type(status) is not int or not 200 <= status <= 299:
        raise ValueError("successful response status must be a 2xx integer")
    return _observation(request_count, method, path, status, "none")


def http_error_observation(
    request_count: int, method: str, path: str, error: HTTPError
) -> dict[str, int | str]:
    """Preserve only HTTPError.code and the fixed HTTPError class label."""
    if type(error) is not HTTPError:
        raise TypeError("error must be urllib.error.HTTPError")
    return _observation(request_count, method, path, error.code, "HTTPError")


def transport_error_observation(
    request_count: int, method: str, path: str, error: URLError | OSError | TimeoutError
) -> dict[str, int | str]:
    """Represent known no-response transport errors with an explicit unknown status."""
    labels = {URLError: "URLError", OSError: "OSError", TimeoutError: "TimeoutError"}
    error_type = labels.get(type(error))
    if error_type is None:
        raise TypeError("error must be a recognized urllib or transport exception")
    return _observation(request_count, method, path, "unknown", error_type)


def render_observation(record: dict[str, int | str]) -> str:
    """Render exactly the allowlisted fields in stable order as key=value lines."""
    if not isinstance(record, dict) or set(record) != set(_FIELDS):
        raise ValueError("observation must contain exactly the allowlisted fields")
    checked = _observation(
        record["request_count"],
        record["method"],
        record["path"],
        record["http_status"],
        record["error_type"],
    )
    return "\n".join(f"{field}={checked[field]}" for field in _FIELDS)
