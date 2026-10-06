#!/usr/bin/env python3
"""Send one sanitized, pre-task NVIDIA Nemotron Chat Completions control."""

from __future__ import annotations

import argparse
import json
import os
import re
import shlex
import stat
import sys
from dataclasses import dataclass
from pathlib import Path
from urllib.error import HTTPError, URLError
from urllib.request import HTTPHandler, HTTPRedirectHandler, Request, build_opener

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from sanitized_http import (  # noqa: E402
    http_error_observation,
    render_observation,
    response_observation,
    transport_error_observation,
)

EXPECTED_ENV_FILE = Path("/home/pedro/.config/runstead-canary/nvidia.env")
BASE_URL = "https://integrate.api.nvidia.com/v1"
MODEL_ID = "nvidia/nemotron-3-super-120b-a12b"
AUTH_REF = "NVIDIA_API_KEY"
ENDPOINT_PATH = "/chat/completions"
OBSERVATION_PATH = "/chat/completions"
TIMEOUT_SECONDS = 30
MAX_RESPONSE_BYTES = 1_048_576


@dataclass(frozen=True)
class Stage1Result:
    observation: dict[str, int | str]
    response_shape_valid: bool
    returned_model_matches: bool

    @property
    def request_count(self) -> int:
        return int(self.observation["request_count"])


class _NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def _payload() -> bytes:
    return json.dumps(
        {
            "model": MODEL_ID,
            "messages": [{"role": "user", "content": "Reply with OK."}],
            "max_tokens": 1,
            "stream": False,
        },
        separators=(",", ":"),
    ).encode("utf-8")


def _validate_response(payload: object) -> tuple[bool, bool]:
    if not isinstance(payload, dict):
        return False, False
    model = payload.get("model")
    model_matches = type(model) is str and model == MODEL_ID
    choices = payload.get("choices")
    if payload.get("object") != "chat.completion" or not isinstance(choices, list) or not choices:
        return False, model_matches
    first = choices[0]
    message = first.get("message") if isinstance(first, dict) else None
    shape_valid = (
        isinstance(message, dict)
        and type(message.get("role")) is str
        and message.get("role") == "assistant"
        and type(message.get("content")) is str
    )
    return shape_valid, model_matches


def _dispatch_once(
    base_url: str,
    model: str,
    api_key: str,
    *,
    opener_factory=build_opener,
) -> Stage1Result:
    """Make one exact request; return only an allowlisted observation and booleans."""
    if model != MODEL_ID or not api_key:
        raise ValueError("fixed Stage 1 contract unavailable")
    request = Request(
        base_url.rstrip("/") + ENDPOINT_PATH,
        data=_payload(),
        headers={
            "Accept": "application/json",
            "Authorization": f"Bearer {api_key}",
            "Content-Type": "application/json",
            "User-Agent": "Runstead-GateA-NVIDIA-Nemotron-3-Super-v1/1.0",
        },
        method="POST",
    )
    opener = opener_factory(_NoRedirect(), HTTPHandler())
    try:
        with opener.open(request, timeout=TIMEOUT_SECONDS) as response:
            status = response.status
            response_body = response.read(MAX_RESPONSE_BYTES + 1)
        observation = response_observation(1, "POST", OBSERVATION_PATH, status)
    except HTTPError as exc:
        observation = http_error_observation(1, "POST", OBSERVATION_PATH, exc)
        exc.close()
        return Stage1Result(observation, False, False)
    except (URLError, OSError, TimeoutError) as exc:
        observation = transport_error_observation(1, "POST", OBSERVATION_PATH, exc)
        return Stage1Result(observation, False, False)

    if len(response_body) > MAX_RESPONSE_BYTES:
        return Stage1Result(observation, False, False)
    try:
        payload = json.loads(response_body)
    except (UnicodeDecodeError, json.JSONDecodeError):
        return Stage1Result(observation, False, False)
    shape_valid, model_matches = _validate_response(payload)
    return Stage1Result(observation, shape_valid, model_matches)


def dispatch_once(api_key: str, *, opener_factory=build_opener) -> Stage1Result:
    """Send the fixed NVIDIA request; endpoint and model are not caller-selectable."""
    return _dispatch_once(BASE_URL, MODEL_ID, api_key, opener_factory=opener_factory)


def load_key_without_emitting(env_file: Path) -> tuple[dict[str, bool], str | None]:
    os.environ.pop(AUTH_REF, None)
    checks = {
        "env_file_regular": False,
        "nvidia_key_declaration_present": False,
        "nvidia_key_loaded": False,
        "nvidia_key_nonempty": False,
        "secret_value_emitted": False,
    }
    try:
        checks["env_file_regular"] = stat.S_ISREG(env_file.stat().st_mode)
        if not checks["env_file_regular"]:
            return checks, None
        content = env_file.read_text(encoding="utf-8")
    except (OSError, UnicodeError):
        return checks, None

    declarations = []
    for line in content.splitlines():
        match = re.match(r"^\s*(?:export\s+)?NVIDIA_API_KEY\s*=\s*(.*?)\s*$", line)
        if match:
            checks["nvidia_key_declaration_present"] = True
            try:
                parsed = shlex.split(match.group(1), comments=True, posix=True)
            except ValueError:
                return checks, None
            declarations.append(parsed[0] if parsed else "")
    if len(declarations) != 1:
        return checks, None

    key = declarations[0]
    if key:
        os.environ[AUTH_REF] = key
    checks["nvidia_key_loaded"] = AUTH_REF in os.environ
    checks["nvidia_key_nonempty"] = bool(os.environ.get(AUTH_REF))
    return checks, os.environ.get(AUTH_REF) if checks["nvidia_key_nonempty"] else None


def render_result(result: Stage1Result) -> str:
    return "\n".join(
        (
            render_observation(result.observation),
            f"response_shape_valid={str(result.response_shape_valid).lower()}",
            f"returned_model_matches={str(result.returned_model_matches).lower()}",
        )
    )


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--env-file", required=True, type=Path)
    parser.add_argument("--dispatch-authorized", action="store_true")
    args = parser.parse_args(argv)
    if args.env_file != EXPECTED_ENV_FILE:
        print("env_file_is_expected_external_reference=false")
        print("request_count=0")
        print("stage1_result=stopped_preflight")
        return 2

    checks, key = load_key_without_emitting(args.env_file)
    for name, value in checks.items():
        print(f"{name}={str(value).lower()}")
    if key is None:
        print("request_count=0")
        print("stage1_result=stopped_preflight")
        return 2
    if not args.dispatch_authorized:
        print("request_count=0")
        print("stage1_result=not_dispatched authorization_required=true")
        os.environ.pop(AUTH_REF, None)
        return 2

    try:
        result = dispatch_once(key)
    finally:
        key = None
        os.environ.pop(AUTH_REF, None)
    print(render_result(result))
    status = result.observation["http_status"]
    passed = (
        type(status) is int
        and 200 <= status < 300
        and result.response_shape_valid
        and result.returned_model_matches
    )
    print(f"stage1_pass={str(passed).lower()}")
    return 0 if passed else 1


if __name__ == "__main__":
    raise SystemExit(main())
