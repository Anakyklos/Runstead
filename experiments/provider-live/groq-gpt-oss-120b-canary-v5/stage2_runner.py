#!/usr/bin/env python3
"""Run exactly one Stage 2 task while suppressing raw CLI/provider output."""

from __future__ import annotations

import argparse
import os
import re
import subprocess
import sys
from pathlib import Path

from stage1_models import load_key_into_process

OBJECTIVE = (
    "Read app/calc.go using the available repository tool. Complete only after citing the actual "
    "read_file observation for app/calc.go. Do not modify files and do not run recipes or processes."
)
PROVIDER_ID = "groq-gpt-oss-120b-canary-v5"


def child_environment(key: str) -> dict[str, str]:
    # Pass only the exact requested provider credential plus basic host/runtime
    # paths. Do not inherit alternate provider keys or ambient Runstead flags.
    allow = {"PATH", "HOME", "TMPDIR", "GOCACHE", "GOMODCACHE", "GOPATH", "GOFLAGS"}
    env = {name: value for name, value in os.environ.items() if name in allow}
    env["GROQ_API_KEY"] = key
    return env


def run(args: argparse.Namespace) -> int:
    key, _source = load_key_into_process()
    command = [
        str(args.bin), "run", "--task", OBJECTIVE,
        "--workspace", str(args.workspace),
        "--providers", str(args.providers), "--provider-id", PROVIDER_ID,
        "--profile", str(args.profile), "--acceptance", str(args.acceptance),
        "--state-dir", str(args.state_dir), "--retry-policy", "off",
        "--max-steps", "12", "--max-verification-retries", "3",
        "--provider-budget", "12", "--time-budget", "10m", "--log-level", "error",
    ]
    try:
        completed = subprocess.run(
            command,
            env=child_environment(key),
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )
    except OSError as exc:
        print(f"runner_error_type={type(exc).__name__}")
        return 2
    # The CLI writes only a task identifier on stderr before running. Never
    # persist or print stdout/stderr: that could contain untrusted model text.
    combined = completed.stdout + b"\n" + completed.stderr
    match = re.search(rb"(?m)^task: (cli-[0-9]+)\s*$", combined)
    print(f"run_exit_code={completed.returncode}")
    print(f"task_id={match.group(1).decode('ascii') if match else 'unavailable'}")
    return completed.returncode


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--bin", required=True, type=Path)
    parser.add_argument("--workspace", required=True, type=Path)
    parser.add_argument("--providers", required=True, type=Path)
    parser.add_argument("--profile", required=True, type=Path)
    parser.add_argument("--acceptance", required=True, type=Path)
    parser.add_argument("--state-dir", required=True, type=Path)
    parser.add_argument("--env-file", required=True, type=Path)
    args = parser.parse_args()
    os.environ["RUNSTEAD_GROQ_ENV_FILE"] = str(args.env_file)
    args.state_dir.mkdir(parents=True, exist_ok=False)
    try:
        return run(args)
    except RuntimeError as exc:
        print(f"runner_error_type={type(exc).__name__}")
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
