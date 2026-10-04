#!/usr/bin/env python3
"""One pre-authorized, policy-safe Stage 3 controlled rerun only."""

from __future__ import annotations

import argparse
import os
import re
import subprocess
from pathlib import Path

from stage1_models import load_key_into_process
from stage3_runner import OBJECTIVE, PROVIDER_ID, child_environment


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--bin", required=True, type=Path)
    parser.add_argument("--workspace", required=True, type=Path)
    parser.add_argument("--providers", required=True, type=Path)
    parser.add_argument("--profile", required=True, type=Path)
    parser.add_argument("--acceptance", required=True, type=Path)
    parser.add_argument("--recipes", required=True, type=Path)
    parser.add_argument("--state-dir", required=True, type=Path)
    parser.add_argument("--env-file", required=True, type=Path)
    args = parser.parse_args()
    os.environ["RUNSTEAD_GROQ_ENV_FILE"] = str(args.env_file)
    if not args.state_dir.is_dir():
        print("rerun_refused=durable_governor_state_missing")
        return 2
    try:
        key, _source = load_key_into_process()
        command = [
            str(args.bin), "run", "--task", OBJECTIVE,
            "--workspace", str(args.workspace),
            "--providers", str(args.providers), "--provider-id", PROVIDER_ID,
            "--profile", str(args.profile), "--acceptance", str(args.acceptance),
            "--recipes", str(args.recipes), "--recipe-policy", "test=allow",
            "--write-policy", "write_file=allow,apply_patch=allow",
            "--state-dir", str(args.state_dir), "--retry-policy", "off",
            "--max-steps", "24", "--max-verification-retries", "3",
            "--provider-budget", "24", "--time-budget", "10m",
            "--min-start-interval", "5s", "--log-level", "error",
        ]
        completed = subprocess.run(
            command,
            env=child_environment(key),
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )
    except Exception as exc:
        print(f"runner_error_type={type(exc).__name__}")
        return 2
    match = re.search(rb"(?m)^task: (cli-[0-9]+)\s*$", completed.stdout + b"\n" + completed.stderr)
    print(f"run_exit_code={completed.returncode}")
    print(f"task_id={match.group(1).decode('ascii') if match else 'unavailable'}")
    return completed.returncode


if __name__ == "__main__":
    raise SystemExit(main())
