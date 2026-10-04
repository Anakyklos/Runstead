#!/usr/bin/env python3
"""Perform at most one authenticated Groq GET /models; emit sanitized facts."""

from __future__ import annotations

import json
import os
import shlex
import sys
from pathlib import Path
from urllib.error import HTTPError, URLError
from urllib.request import HTTPRedirectHandler, Request, build_opener

BASE_URL = "https://api.groq.com/openai/v1"
EXPECTED_MODEL = "openai/gpt-oss-120b"
USER_AGENT = "Runstead-GateA-Canary/1.0"


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, request, response, code, message, headers, new_url):
        return None


def load_key_into_process() -> tuple[str, str]:
    key = os.environ.get("GROQ_API_KEY", "").strip()
    if key:
        return key, "process_environment"
    env_file = os.environ.get("RUNSTEAD_GROQ_ENV_FILE", "").strip()
    if not env_file:
        raise RuntimeError("GROQ_API_KEY is absent")
    path = Path(env_file)
    if not path.is_file():
        raise RuntimeError("GROQ_API_KEY is absent")
    for line in path.read_text(encoding="utf-8").splitlines():
        value = line.strip()
        if value.startswith("export "):
            value = value[7:].lstrip()
        if not value.startswith("GROQ_API_KEY="):
            continue
        raw = value.split("=", 1)[1].strip()
        try:
            parsed = shlex.split(raw, comments=True, posix=True)
        except ValueError as exc:
            raise RuntimeError("GROQ_API_KEY could not be loaded") from exc
        if not parsed or not parsed[0]:
            break
        os.environ["GROQ_API_KEY"] = parsed[0]
        return parsed[0], "ignored_env_file_loaded_into_process"
    raise RuntimeError("GROQ_API_KEY is absent")


def request_model_presence(key: str, opener_factory=build_opener) -> tuple[int, bool]:
    request = Request(
        BASE_URL + "/models",
        headers={
            "Accept": "application/json",
            "Authorization": "Bearer " + key,
            "User-Agent": USER_AGENT,
        },
        method="GET",
    )
    with opener_factory(NoRedirect).open(request, timeout=30) as response:
        status = response.status
        payload = json.load(response)
    if status != 200:
        raise RuntimeError("model-list request did not return HTTP 200")
    models = payload.get("data") if isinstance(payload, dict) else None
    if not isinstance(models, list):
        raise RuntimeError("model-list response had no data array")
    present = any(
        isinstance(item, dict) and item.get("id") == EXPECTED_MODEL
        for item in models
    )
    return status, present


def main() -> int:
    request_count = 0
    try:
        key, key_source = load_key_into_process()
        request_count = 1
        status, present = request_model_presence(key)
        print(f"request_count={request_count}")
        print(f"method=GET path=/models user_agent={USER_AGENT}")
        print(f"http_status={status}")
        print(f"exact_model_present={str(present).lower()}")
        print(f"secret_loaded_from={key_source}")
        return 0 if present else 1
    except (HTTPError, URLError, OSError, RuntimeError, json.JSONDecodeError) as exc:
        # Never render exception bodies, headers, or credential data.
        if request_count == 0 and os.environ.get("GROQ_API_KEY", "").strip():
            request_count = 1
        print(f"request_count={request_count}")
        print(f"stage1_request_result=failed error_type={type(exc).__name__}")
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
