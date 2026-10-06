#!/usr/bin/env python3
"""Run one frozen Stage 2 or Stage 3 task without retaining raw CLI output."""

from __future__ import annotations

import argparse
import os
import re
import subprocess
from dataclasses import dataclass
from pathlib import Path

from stage1_control import (
    AUTH_REF,
    EXPECTED_ENV_FILE,
    load_key_without_emitting,
)
from stage2_audit import OBJECTIVE as STAGE2_OBJECTIVE
from stage3_audit import OBJECTIVE as STAGE3_OBJECTIVE

PROVIDER_ID = "nvidia-nim-nemotron-3-super-120b-a12b-canary-v1"
_SAFE_ENV = {"PATH", "HOME", "TMPDIR", "GOCACHE", "GOMODCACHE", "GOPATH", "GOFLAGS"}


@dataclass(frozen=True)
class RunArgs:
    binary: str
    workspace: str
    providers: str
    stage2_profile: str
    stage3_profile: str
    stage2_acceptance: str
    stage3_acceptance: str
    recipes: str
    state_dir: str
    env_file: Path


def child_environment(key: str, source: dict[str, str] | None = None) -> dict[str, str]:
    source = os.environ if source is None else source
    child = {name: value for name, value in source.items() if name in _SAFE_ENV}
    child[AUTH_REF] = key
    return child


def build_command(stage: str, args: RunArgs) -> list[str]:
    if stage == "stage2":
        objective = STAGE2_OBJECTIVE
        profile = args.stage2_profile
        acceptance = args.stage2_acceptance
        limits = ("12", "12")
        stage_args: list[str] = []
    elif stage == "stage3":
        objective = STAGE3_OBJECTIVE
        profile = args.stage3_profile
        acceptance = args.stage3_acceptance
        limits = ("24", "24")
        stage_args = [
            "--recipes", args.recipes,
            "--recipe-policy", "test=allow",
            "--write-policy", "write_file=allow,apply_patch=allow",
        ]
    else:
        raise ValueError("unsupported_stage")

    command = [
        args.binary, "run", "--task", objective,
        "--workspace", args.workspace,
        "--providers", args.providers,
        "--provider-id", PROVIDER_ID,
        "--profile", profile,
        "--acceptance", acceptance,
        "--state-dir", args.state_dir,
        "--retry-policy", "off",
        "--max-steps", limits[0],
        "--max-verification-retries", "3",
        "--provider-budget", limits[1],
        "--time-budget", "10m",
        "--log-level", "error",
    ]
    return command + stage_args


def _task_id(output: bytes) -> str:
    match = re.search(rb"(?m)^task: (cli-[0-9]+)\s*$", output)
    return match.group(1).decode("ascii") if match else "unavailable"


def _run(stage: str, args: RunArgs) -> int:
    if args.env_file != EXPECTED_ENV_FILE:
        print("env_file_is_expected_external_reference=false")
        print("run_exit_code=not_started")
        print("task_id=unavailable")
        return 2
    checks, key = load_key_without_emitting(args.env_file)
    for name, value in checks.items():
        print(f"{name}={str(value).lower()}")
    if key is None:
        print("run_exit_code=not_started")
        print("task_id=unavailable")
        return 2

    child = child_environment(key)
    os.environ.pop(AUTH_REF, None)
    key = None
    try:
        Path(args.state_dir).mkdir(parents=True, exist_ok=False)
        completed = subprocess.run(
            build_command(stage, args),
            env=child,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )
    except OSError as exc:
        print(f"runner_error_type={type(exc).__name__}")
        return 2
    finally:
        child.pop(AUTH_REF, None)

    task_id = _task_id(completed.stdout + b"\n" + completed.stderr)
    print(f"run_exit_code={completed.returncode}")
    print(f"task_id={task_id}")
    return completed.returncode


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--stage", choices=("stage2", "stage3"), required=True)
    parser.add_argument("--bin", dest="binary", required=True)
    parser.add_argument("--workspace", required=True)
    parser.add_argument("--providers", required=True)
    parser.add_argument("--stage2-profile", required=True)
    parser.add_argument("--stage3-profile", required=True)
    parser.add_argument("--stage2-acceptance", required=True)
    parser.add_argument("--stage3-acceptance", required=True)
    parser.add_argument("--recipes", required=True)
    parser.add_argument("--state-dir", required=True)
    parser.add_argument("--env-file", required=True, type=Path)
    parsed = parser.parse_args()
    stage = parsed.stage
    del parsed.stage
    args = RunArgs(**vars(parsed))
    return _run(stage, args)


if __name__ == "__main__":
    raise SystemExit(main())
