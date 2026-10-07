#!/usr/bin/env python3
"""Send one sanitized, pre-task NVIDIA DeepSeek V4.1 Flash Chat Completions control."""

from __future__ import annotations

import argparse
import json
import os
import re
import shlex
import stat
import subprocess
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
HERE = Path(__file__).resolve().parent
REPO_ROOT = HERE.parents[2]
BASE_URL = "https://integrate.api.nvidia.com/v1"
MODEL_ID = "deepseek-ai/deepseek-v4.1-flash"
AUTH_REF = "NVIDIA_API_KEY"
ENDPOINT_PATH = "/chat/completions"
OBSERVATION_PATH = "/chat/completions"
TIMEOUT_SECONDS = 30
MAX_RESPONSE_BYTES = 1_048_576
_RESOLVER_SUCCESS = (
    "provider_id=nvidia-nim-deepseek-v4-1-flash-canary-v1\n"
    "protocol_family=openai_compatible\n"
    "base_url=https://integrate.api.nvidia.com/v1\n"
    "model=deepseek-ai/deepseek-v4.1-flash\n"
    "auth_requirement=reference_required\n"
    "auth_ref=NVIDIA_API_KEY\n"
    "route_safety=SafeRouteSafety\n"
    "adapter_constructed=false\n"
    "provider_requests=0\n"
)


@dataclass(frozen=True)
class ResponseDiagnostics:
    json_object_valid: bool
    completion_object_valid: bool
    choices_valid: bool
    message_valid: bool
    assistant_role_valid: bool
    content_type_valid: bool
    returned_model_matches: bool

    @property
    def compatible_shape_valid(self) -> bool:
        return all((
            self.json_object_valid,
            self.completion_object_valid,
            self.choices_valid,
            self.message_valid,
            self.assistant_role_valid,
            self.content_type_valid,
        ))


@dataclass(frozen=True)
class Stage1Result:
    observation: dict[str, int | str]
    diagnostics: ResponseDiagnostics

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
            "max_tokens": 64,
            "stream": False,
        },
        separators=(",", ":"),
    ).encode("utf-8")


def _invalid_diagnostics() -> ResponseDiagnostics:
    return ResponseDiagnostics(False, False, False, False, False, False, False)


def _validate_response(payload: object) -> ResponseDiagnostics:
    if not isinstance(payload, dict):
        return _invalid_diagnostics()
    model = payload.get("model")
    model_matches = type(model) is str and model == MODEL_ID

    completion_object = payload.get("object")
    completion_object_valid = (
        "object" not in payload
        or (type(completion_object) is str and completion_object == "chat.completion")
    )

    choices = payload.get("choices")
    choices_valid = (
        isinstance(choices, list)
        and bool(choices)
        and isinstance(choices[0], dict)
    )
    first_choice = choices[0] if choices_valid else None
    message = first_choice.get("message") if isinstance(first_choice, dict) else None
    message_valid = isinstance(message, dict)
    role = message.get("role") if message_valid else None
    assistant_role_valid = type(role) is str and role == "assistant"
    content = message.get("content") if message_valid else object()
    content_type_valid = "content" in message and (type(content) is str or content is None) if message_valid else False

    return ResponseDiagnostics(
        json_object_valid=True,
        completion_object_valid=completion_object_valid,
        choices_valid=choices_valid,
        message_valid=message_valid,
        assistant_role_valid=assistant_role_valid,
        content_type_valid=content_type_valid,
        returned_model_matches=model_matches,
    )


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
            "User-Agent": "Runstead-GateA-NVIDIA-DeepSeek-V4.1-Flash-v1/1.0",
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
        return Stage1Result(observation, _invalid_diagnostics())
    except (URLError, OSError, TimeoutError) as exc:
        observation = transport_error_observation(1, "POST", OBSERVATION_PATH, exc)
        return Stage1Result(observation, _invalid_diagnostics())

    if len(response_body) > MAX_RESPONSE_BYTES:
        return Stage1Result(observation, _invalid_diagnostics())
    try:
        payload = json.loads(response_body)
    except (UnicodeDecodeError, json.JSONDecodeError):
        return Stage1Result(observation, _invalid_diagnostics())
    diagnostics = _validate_response(payload)
    return Stage1Result(observation, diagnostics)


def dispatch_once(api_key: str, *, opener_factory=build_opener) -> Stage1Result:
    """Send the fixed NVIDIA request; endpoint and model are not caller-selectable."""
    return _dispatch_once(BASE_URL, MODEL_ID, api_key, opener_factory=opener_factory)


def resolve_provider_config(
    *, config_path: Path | None = None, runner=subprocess.run,
) -> bool:
    """Resolve only the fixed static contract in a child without the auth secret."""
    env = os.environ.copy()
    env.pop(AUTH_REF, None)
    env["GOPROXY"] = "off"
    env["GOTOOLCHAIN"] = "local"
    argv = ["go", "run", str(HERE / "stage1_resolve.go"), str(config_path or HERE / "providers.json")]
    try:
        result = runner(
            argv,
            cwd=REPO_ROOT,
            env=env,
            capture_output=True,
            text=True,
            check=False,
            shell=False,
        )
    except (OSError, subprocess.SubprocessError):
        return False
    return result.returncode == 0 and result.stdout == _RESOLVER_SUCCESS and result.stderr == ""


def load_key_without_emitting(env_file: Path) -> tuple[dict[str, bool], str | None]:
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
        checks["nvidia_key_loaded"] = True
    checks["nvidia_key_nonempty"] = bool(key)
    return checks, key if checks["nvidia_key_nonempty"] else None


def render_result(result: Stage1Result) -> str:
    return "\n".join(
        (
            render_observation(result.observation),
            f"json_object_valid={str(result.diagnostics.json_object_valid).lower()}",
            f"completion_object_valid={str(result.diagnostics.completion_object_valid).lower()}",
            f"choices_valid={str(result.diagnostics.choices_valid).lower()}",
            f"message_valid={str(result.diagnostics.message_valid).lower()}",
            f"assistant_role_valid={str(result.diagnostics.assistant_role_valid).lower()}",
            f"content_type_valid={str(result.diagnostics.content_type_valid).lower()}",
            f"returned_model_matches={str(result.diagnostics.returned_model_matches).lower()}",
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

    if not resolve_provider_config():
        print("provider_config_resolved=false")
        print("request_count=0")
        print("stage1_result=stopped_preflight")
        return 2

    if not args.dispatch_authorized:
        print("request_count=0")
        print("stage1_result=not_dispatched authorization_required=true")
        return 2

    checks, key = load_key_without_emitting(args.env_file)
    for name, value in checks.items():
        print(f"{name}={str(value).lower()}")
    if key is None:
        print("request_count=0")
        print("stage1_result=stopped_preflight")
        return 2
    try:
        result = dispatch_once(key)
    finally:
        key = None
    print(render_result(result))
    status = result.observation["http_status"]
    passed = (
        type(status) is int
        and 200 <= status < 300
        and result.diagnostics.compatible_shape_valid
        and result.diagnostics.returned_model_matches
    )
    print(f"stage1_pass={str(passed).lower()}")
    return 0 if passed else 1


if __name__ == "__main__":
    raise SystemExit(main())
