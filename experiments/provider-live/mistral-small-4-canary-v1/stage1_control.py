"""Single, sanitized Mistral Stage 1 control for the bounded Gate A canary."""

from __future__ import annotations

import argparse
import json
import os
import re
import stat
import sys
from dataclasses import dataclass
from pathlib import Path
from urllib.error import HTTPError, URLError
from urllib.request import HTTPHandler, HTTPRedirectHandler, Request, build_opener

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from sanitized_http import (
    http_error_observation,
    render_observation,
    response_observation,
    transport_error_observation,
)

PROVIDER_ID = "mistral-small-4-canary-v1"
BASE_URL = "https://api.mistral.ai/v1"
MODEL_ID = "mistral-small-2603"
AUTH_REF = "MISTRAL_API_KEY"
PATH = "/v1/chat/completions"
_ENDPOINT_PATH = "/chat/completions"
_TIMEOUT_SECONDS = 30
_MAX_RESPONSE_BYTES = 1_048_576


@dataclass(frozen=True)
class Stage1Result:
    observation: dict[str, int | str]
    response_shape_valid: bool
    returned_model_matches: bool

    @property
    def request_count(self) -> int:
        return int(self.observation["request_count"])


def _valid_chat_completion(payload: object) -> tuple[bool, bool]:
    if not isinstance(payload, dict):
        return False, False
    returned_model = payload.get("model")
    model_matches = type(returned_model) is str and returned_model == MODEL_ID
    choices = payload.get("choices")
    if not isinstance(choices, list) or not choices:
        return False, model_matches
    first = choices[0]
    if not isinstance(first, dict):
        return False, model_matches
    message = first.get("message")
    if not isinstance(message, dict):
        return False, model_matches
    valid = (
        payload.get("object") == "chat.completion"
        and type(returned_model) is str
        and type(message.get("role")) is str
        and message.get("role") == "assistant"
        and type(message.get("content")) is str
    )
    return valid, model_matches


class _NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def dispatch_once(base_url: str, model: str, api_key: str) -> Stage1Result:
    """Send exactly one minimal Chat Completions request and discard its body."""
    if model != MODEL_ID or not api_key:
        raise ValueError("fixed Stage 1 contract unavailable")
    body = json.dumps(
        {
            "model": MODEL_ID,
            "messages": [{"role": "user", "content": "Reply with OK."}],
            "max_tokens": 1,
            "stream": False,
        },
        separators=(",", ":"),
    ).encode("utf-8")
    request = Request(
        base_url.rstrip("/") + _ENDPOINT_PATH,
        data=body,
        headers={"Authorization": f"Bearer {api_key}", "Content-Type": "application/json"},
        method="POST",
    )
    opener = build_opener(_NoRedirect(), HTTPHandler())
    try:
        with opener.open(request, timeout=_TIMEOUT_SECONDS) as response:
            status = response.status
            response_body = response.read(_MAX_RESPONSE_BYTES + 1)
        observation = response_observation(1, "POST", PATH, status)
    except HTTPError as exc:
        observation = http_error_observation(1, "POST", PATH, exc)
        exc.close()
        return Stage1Result(observation, False, False)
    except (URLError, OSError, TimeoutError) as exc:
        observation = transport_error_observation(1, "POST", PATH, exc)
        return Stage1Result(observation, False, False)

    if len(response_body) > _MAX_RESPONSE_BYTES:
        return Stage1Result(observation, False, False)
    try:
        payload = json.loads(response_body)
    except (UnicodeDecodeError, json.JSONDecodeError):
        return Stage1Result(observation, False, False)
    shape_valid, model_matches = _valid_chat_completion(payload)
    return Stage1Result(observation, shape_valid, model_matches)


def _load_key_without_emitting(env_file: Path) -> tuple[dict[str, bool], str | None]:
    regular = False
    declaration_present = False
    loaded = False
    nonempty = False
    key = None
    try:
        regular = stat.S_ISREG(env_file.stat().st_mode)
        if regular:
            matches = []
            for raw_line in env_file.read_text(encoding="utf-8").splitlines():
                line = raw_line.strip()
                if not line or line.startswith("#"):
                    continue
                if line.startswith("export "):
                    line = line[7:].lstrip()
                match = re.match(r"^([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*)$", line)
                if match and match.group(1) == AUTH_REF:
                    matches.append(match.group(2).strip())
            if len(matches) == 1:
                declaration_present = True
                key = matches[0]
                if len(key) >= 2 and key[0] == key[-1] and key[0] in "'\"":
                    key = key[1:-1]
                else:
                    key = key.split("#", 1)[0].strip()
                os.environ[AUTH_REF] = key
                loaded = AUTH_REF in os.environ
                nonempty = loaded and bool(os.environ[AUTH_REF])
    except (OSError, UnicodeError):
        pass
    return {
        "env_file_regular": regular,
        "mistral_key_declaration_present": declaration_present,
        "mistral_key_loaded": loaded,
        "mistral_key_nonempty": nonempty,
        "secret_value_emitted": False,
    }, key if nonempty else None


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
    args = parser.parse_args(argv)
    booleans, key = _load_key_without_emitting(args.env_file)
    for name, value in booleans.items():
        print(f"{name}={str(value).lower()}")
    if key is None:
        print("provider_requests=0")
        return 2
    try:
        result = dispatch_once(BASE_URL, MODEL_ID, key)
    finally:
        key = None
        os.environ.pop(AUTH_REF, None)
    status = result.observation["http_status"]
    passed = (
        result.response_shape_valid
        and result.returned_model_matches
        and type(status) is int
        and 200 <= status < 300
    )
    print(render_result(result))
    print(f"stage1_pass={str(passed).lower()}")
    return 0 if passed else 1


if __name__ == "__main__":
    sys.exit(main())
