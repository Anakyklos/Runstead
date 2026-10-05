#!/usr/bin/env python3
"""Make at most one exact NVIDIA NIM chat-completions control; emit sanitized facts."""

from __future__ import annotations

import argparse
import json
import os
import re
import shlex
import stat
import sys
from pathlib import Path
from urllib.error import HTTPError, URLError
from urllib.request import HTTPRedirectHandler, Request, build_opener

EXPECTED_ENV_FILE = "/home/pedro/.config/runstead-canary/nvidia.env"
BASE_URL = "https://integrate.api.nvidia.com/v1"
EXPECTED_MODEL = "poolside/laguna-xs-2.1"
USER_AGENT = "Runstead-GateA-NVIDIA-Laguna-XS-2.1-v1/1.0"


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, request, response, code, message, new_url):
        return None


def load_key_from_explicit_file(path_text: str) -> tuple[str, dict[str, bool]]:
    path = Path(path_text)
    checks = {
        "env_file_path_matches_expected": path_text == EXPECTED_ENV_FILE,
        "env_file_regular": False,
        "nvidia_key_declaration_present": False,
        "nvidia_key_loaded": False,
        "nvidia_key_nonempty": False,
        "secret_value_emitted": False,
    }
    try:
        info = path.stat()
        checks["env_file_regular"] = stat.S_ISREG(info.st_mode)
        if not checks["env_file_regular"]:
            return "", checks
        contents = path.read_text(encoding="utf-8")
    except (OSError, UnicodeError):
        return "", checks

    declaration = re.compile(r"^\s*(?:export\s+)?NVIDIA_API_KEY\s*=", re.MULTILINE)
    checks["nvidia_key_declaration_present"] = bool(declaration.search(contents))
    value = None
    for line in contents.splitlines():
        candidate = line.strip()
        if candidate.startswith("export "):
            candidate = candidate[7:].lstrip()
        if not candidate.startswith("NVIDIA_API_KEY="):
            continue
        raw = candidate.split("=", 1)[1].strip()
        try:
            parsed = shlex.split(raw, comments=True, posix=True)
        except ValueError:
            return "", checks
        if parsed:
            value = parsed[0]
    if value is not None:
        os.environ["NVIDIA_API_KEY"] = value
    checks["nvidia_key_loaded"] = "NVIDIA_API_KEY" in os.environ
    checks["nvidia_key_nonempty"] = bool(os.environ.get("NVIDIA_API_KEY"))
    return os.environ.get("NVIDIA_API_KEY", ""), checks


def request_exact_model(key: str, opener_factory=build_opener) -> tuple[int, bool]:
    payload = json.dumps(
        {
            "model": EXPECTED_MODEL,
            "messages": [{"role": "user", "content": "Reply with OK."}],
            "max_tokens": 1,
            "stream": False,
        }
    ).encode("utf-8")
    request = Request(
        BASE_URL + "/chat/completions",
        data=payload,
        headers={
            "Accept": "application/json",
            "Authorization": "Bearer " + key,
            "Content-Type": "application/json",
            "User-Agent": USER_AGENT,
        },
        method="POST",
    )
    with opener_factory(NoRedirect).open(request, timeout=60) as response:
        status = response.status
        raw = response.read(2_000_001)
    if status != 200 or len(raw) > 2_000_000:
        return status, False
    decoded = json.loads(raw)
    if not isinstance(decoded, dict) or decoded.get("model") != EXPECTED_MODEL:
        return status, False
    choices = decoded.get("choices")
    valid = (
        isinstance(choices, list)
        and len(choices) >= 1
        and isinstance(choices[0], dict)
        and isinstance(choices[0].get("message"), dict)
    )
    return status, valid


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--env-file", required=True)
    parser.add_argument("--dispatch-authorized", action="store_true")
    args = parser.parse_args()

    # This command is intended to run in a minimal isolated environment. Do
    # not use a pre-inherited NVIDIA_API_KEY or RUNSTEAD_NVIDIA_ENV_FILE.
    key, checks = load_key_from_explicit_file(args.env_file)
    for name, value in checks.items():
        print(f"{name}={str(value).lower()}")
    if not all(value for name, value in checks.items() if name != "secret_value_emitted"):
        print("request_count=0")
        print("stage1_result=stopped_preflight")
        return 2
    if not args.dispatch_authorized:
        print("request_count=0")
        print("stage1_result=not_dispatched authorization_required=true")
        return 2

    try:
        status, valid = request_exact_model(key)
        print("request_count=1")
        print("method=POST path=/v1/chat/completions")
        print(f"http_status={status}")
        print(f"response_exact_model_valid={str(valid).lower()}")
        print("secret_loaded_from=external_env_file")
        return 0 if status == 200 and valid else 1
    except (HTTPError, URLError, OSError, TimeoutError, json.JSONDecodeError, ValueError) as exc:
        # Never print exception text, response bodies, headers, or credential data.
        print("request_count=1")
        print(f"stage1_result=failed error_type={type(exc).__name__}")
        print("secret_loaded_from=external_env_file")
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
