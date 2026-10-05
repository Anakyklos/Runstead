#!/usr/bin/env python3
"""Run one exact NVIDIA NIM control and emit only sanitized HTTP facts."""

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

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from sanitized_http import (  # noqa: E402
    http_error_observation,
    render_observation,
    response_observation,
    transport_error_observation,
)

EXPECTED_ENV_FILE = Path("/home/pedro/.config/runstead-canary/nvidia.env")
BASE_URL = "https://integrate.api.nvidia.com/v1"
EXPECTED_MODEL = "poolside/laguna-xs-2.1"
PATH = "/chat/completions"
USER_AGENT = "Runstead-GateA-NVIDIA-Laguna-XS-2.1-v2/1.0"


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, request, response, code, message, new_url):
        return None


def load_key_from_explicit_file(path: Path, inherited_key: str | None = None):
    os.environ.pop("NVIDIA_API_KEY", None)
    checks = {
        "env_file_regular": False,
        "nvidia_key_declaration_present": False,
        "nvidia_key_loaded": False,
        "nvidia_key_nonempty": False,
        "secret_value_emitted": False,
    }
    try:
        checks["env_file_regular"] = stat.S_ISREG(path.stat().st_mode)
        if not checks["env_file_regular"]:
            return "", checks
        content = path.read_text(encoding="utf-8")
    except (OSError, UnicodeError):
        return "", checks

    declarations = []
    for line in content.splitlines():
        match = re.match(r"^\s*(?:export\s+)?NVIDIA_API_KEY\s*=\s*(.*?)\s*$", line)
        if match:
            checks["nvidia_key_declaration_present"] = True
            try:
                parsed = shlex.split(match.group(1), comments=True, posix=True)
            except ValueError:
                return "", checks
            declarations.append(parsed[0] if parsed else "")
    if len(declarations) != 1:
        return "", checks
    key = declarations[0]
    if key:
        os.environ["NVIDIA_API_KEY"] = key
    checks["nvidia_key_loaded"] = "NVIDIA_API_KEY" in os.environ
    checks["nvidia_key_nonempty"] = bool(os.environ.get("NVIDIA_API_KEY"))
    return os.environ.get("NVIDIA_API_KEY", ""), checks


def _payload() -> bytes:
    return json.dumps(
        {
            "model": EXPECTED_MODEL,
            "messages": [{"role": "user", "content": "Reply with OK."}],
            "max_tokens": 1,
            "stream": False,
        },
        separators=(",", ":"),
    ).encode("utf-8")


def request_exact_model(key: str, opener_factory=build_opener):
    request = Request(
        BASE_URL + PATH,
        data=_payload(),
        headers={
            "Accept": "application/json",
            "Authorization": "Bearer " + key,
            "Content-Type": "application/json",
            "User-Agent": USER_AGENT,
        },
        method="POST",
    )
    try:
        with opener_factory(NoRedirect).open(request, timeout=60) as response:
            status = response.status
            body = response.read(2_000_001)
        observation = response_observation(1, "POST", PATH, status)
        if not 200 <= status <= 299 or len(body) > 2_000_000:
            return observation, False
        try:
            decoded = json.loads(body)
        except (json.JSONDecodeError, UnicodeDecodeError):
            return observation, False
        choices = decoded.get("choices") if isinstance(decoded, dict) else None
        valid = (
            isinstance(decoded, dict)
            and decoded.get("model") == EXPECTED_MODEL
            and isinstance(choices, list)
            and len(choices) >= 1
            and isinstance(choices[0], dict)
            and isinstance(choices[0].get("message"), dict)
        )
        return observation, valid
    except HTTPError as exc:
        return http_error_observation(1, "POST", PATH, exc), False
    except (URLError, OSError, TimeoutError) as exc:
        return transport_error_observation(1, "POST", PATH, exc), False


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--env-file", required=True, type=Path)
    parser.add_argument("--dispatch-authorized", action="store_true")
    args = parser.parse_args()
    key, checks = load_key_from_explicit_file(args.env_file, os.environ.get("NVIDIA_API_KEY"))
    for name, value in checks.items():
        print(f"{name}={str(value).lower()}")
    if not all(value for name, value in checks.items() if name != "secret_value_emitted"):
        print("request_count=0")
        print("stage1_result=stopped_preflight")
        return 2
    if args.env_file != EXPECTED_ENV_FILE:
        print("request_count=0")
        print("stage1_result=stopped_preflight")
        return 2
    if not args.dispatch_authorized:
        print("request_count=0")
        print("stage1_result=not_dispatched authorization_required=true")
        return 2
    observation, valid = request_exact_model(key)
    print(render_observation(observation))
    print(f"response_exact_model_valid={str(valid).lower()}")
    print("secret_loaded_from=external_env_file")
    return 0 if observation["http_status"] == 200 and valid else 1


if __name__ == "__main__":
    raise SystemExit(main())
