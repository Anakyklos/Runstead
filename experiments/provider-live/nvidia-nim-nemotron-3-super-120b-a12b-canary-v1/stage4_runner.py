#!/usr/bin/env python3
"""Interrupt after durable Stage 4 progress, inspect, and resume one task."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import signal
import sqlite3
import subprocess
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import stage3_audit
from stage1_control import AUTH_REF, EXPECTED_ENV_FILE, load_key_without_emitting
from stage_task_runner import PROVIDER_ID, RunArgs, build_command, child_environment

MODEL = "nvidia/nemotron-3-super-120b-a12b"
EXPECTED_FIX_RELATIVE = Path("fixtures/coding-loop/fixes/calc-correct.go")


@dataclass(frozen=True)
class Stage4Args:
    binary: str
    workspace: Path
    providers: str
    profile: str
    acceptance: str
    recipes: str
    state_dir: Path
    env_file: Path
    expected_fix: Path
    max_wait_seconds: int = 620


def checkpoint_ready(snapshot: dict[str, Any]) -> bool:
    return (
        snapshot.get("task_status") == "running"
        and snapshot.get("objective_matches") is True
        and snapshot.get("target_write_durable") is True
        and snapshot.get("target_hash_matches") is True
        and snapshot.get("successful_test_recipe_durable") is True
        and snapshot.get("verification_attempts") == 0
        and snapshot.get("unsettled_provider_attempts") == 0
        and snapshot.get("acceptance_digest") not in (None, "", "unknown")
    )


def _read_snapshot(state_dir: Path, workspace: Path, expected_fix: Path) -> dict[str, Any] | None:
    database = state_dir / "runstead.db"
    if not database.is_file():
        return None
    conn = sqlite3.connect(database.as_uri() + "?mode=ro", uri=True, timeout=1)
    conn.row_factory = sqlite3.Row
    try:
        conn.execute("PRAGMA query_only=ON")
        if conn.execute("PRAGMA query_only").fetchone()[0] != 1:
            raise ValueError("query_only_unavailable")
        tasks = conn.execute(
            "SELECT task_id,objective,status,workspace,model,config_json,resume_count,execution_contract_hash "
            "FROM tasks WHERE objective=?", (stage3_audit.OBJECTIVE,)
        ).fetchall()
        if len(tasks) != 1:
            return None
        task = tasks[0]
        attempts = conn.execute(
            "SELECT execution_id,client_request_id,status,delivery_state,upstream_reached,uncertain,attempt_debited,attempt_sequence "
            "FROM provider_attempts WHERE task_id=? ORDER BY attempt_sequence", (task["task_id"],)
        ).fetchall()
        tool_attempts = conn.execute(
            "SELECT execution_id,tool,status,evidence_id,effect_after_hash FROM tool_attempts "
            "WHERE task_id=? ORDER BY created_at,execution_id", (task["task_id"],)
        ).fetchall()
        results = conn.execute(
            "SELECT r.evidence_id,r.success,r.data_json,r.metadata_json,t.execution_id,t.tool,t.status "
            "FROM tool_results r JOIN tool_attempts t ON t.execution_id=r.execution_id "
            "WHERE r.task_id=? ORDER BY r.created_at,r.evidence_id", (task["task_id"],)
        ).fetchall()
        writes = []
        recipes = []
        evidence_ids = []
        for row in results:
            if row["success"]:
                evidence_ids.append(row["evidence_id"])
            if not row["success"]:
                continue
            try:
                data = json.loads(row["data_json"])
                metadata = json.loads(row["metadata_json"])
            except (TypeError, json.JSONDecodeError):
                continue
            if row["tool"] in {"write_file", "apply_patch"}:
                path = data.get("path", metadata.get("path", "")) if isinstance(data, dict) and isinstance(metadata, dict) else ""
                writes.append({
                    "execution_id": row["execution_id"],
                    "tool": row["tool"],
                    "status": row["status"],
                    "path": path,
                    "evidence_id": row["evidence_id"],
                })
            elif row["tool"] == "run_recipe":
                recipes.append({
                    "execution_id": row["execution_id"],
                    "passed": stage3_audit.recipe_passed(row["data_json"]),
                    "evidence_id": row["evidence_id"],
                })

        verifications = conn.execute(
            "SELECT sequence,decision FROM verification_attempts WHERE task_id=? ORDER BY sequence",
            (task["task_id"],),
        ).fetchall()
        ledger = conn.execute(
            "SELECT COUNT(*) FROM governor_ledger WHERE task_id=?", (task["task_id"],)
        ).fetchone()[0]
        governor = conn.execute(
            "SELECT attempts,retries FROM governor_task_states WHERE task_id=?", (task["task_id"],)
        ).fetchone()
        acceptance = conn.execute(
            "SELECT digest FROM acceptance_plans WHERE task_id=?", (task["task_id"],)
        ).fetchall()
        config_json = task["config_json"]
        config = json.loads(config_json)
        config_hash = "sha256:" + hashlib.sha256(config_json.encode("utf-8")).hexdigest()
        contract_hash = task["execution_contract_hash"]
        attempt_rows = [dict(row) for row in attempts]
        unsettled = sum(
            row["status"] in {"planned", "prepared", "running", "uncertain", "human_review_required"}
            or bool(row["uncertain"])
            or row["delivery_state"] == "sent_unconfirmed"
            for row in attempt_rows
        )
        file_path = workspace / "app/calc.go"
        target_hash = (
            hashlib.sha256(file_path.read_bytes()).hexdigest()
            if file_path.is_file()
            else "missing"
        )
        expected_hash = hashlib.sha256(expected_fix.read_bytes()).hexdigest()
        successful_target_writes = [
            item for item in writes if item["path"] == "app/calc.go" and item["status"] == "completed"
        ]
        successful_recipes = [item for item in recipes if item["passed"]]
        return {
            "task_id": task["task_id"],
            "task_status": task["status"],
            "objective_matches": task["objective"] == stage3_audit.OBJECTIVE,
            "workspace_matches": Path(task["workspace"]).resolve() == workspace.resolve(),
            "model_matches": task["model"] == MODEL,
            "provider_id_matches": config.get("provider_id") == PROVIDER_ID,
            "provider_model_matches": config.get("provider_model") == MODEL,
            "provider_protocol_matches": config.get("protocol_family") == "openai_compatible",
            "config_sha256": config_hash,
            "execution_contract_hash": contract_hash,
            "acceptance_digest": acceptance[0]["digest"] if len(acceptance) == 1 else "unknown",
            "resume_count": task["resume_count"],
            "provider_attempts": attempt_rows,
            "provider_attempt_count": len(attempt_rows),
            "governor_admissions": ledger,
            "attempt_debits": sum(int(row["attempt_debited"]) for row in attempts),
            "governor_attempts": governor["attempts"] if governor else None,
            "governor_retries": governor["retries"] if governor else None,
            "unsettled_provider_attempts": unsettled,
            "tool_attempts": [dict(row) for row in tool_attempts],
            "writes": writes,
            "successful_target_writes": len(successful_target_writes),
            "target_write_durable": bool(successful_target_writes),
            "target_hash": target_hash,
            "expected_hash": expected_hash,
            "target_hash_matches": target_hash == expected_hash,
            "recipes": recipes,
            "successful_test_recipe_durable": bool(successful_recipes),
            "evidence_ids": evidence_ids,
            "verification_attempts": len(verifications),
            "latest_verification": verifications[-1]["decision"] if verifications else "none",
        }
    finally:
        conn.close()


def _write_snapshot(path: Path, snapshot: dict[str, Any]) -> None:
    with path.open("x", encoding="utf-8") as stream:
        json.dump(snapshot, stream, indent=2, sort_keys=True)
        stream.write("\n")


def _stage4_run_command(args: Stage4Args) -> list[str]:
    return build_command("stage3", RunArgs(
        binary=args.binary,
        workspace=str(args.workspace),
        providers=args.providers,
        stage2_profile="",
        stage3_profile=args.profile,
        stage2_acceptance="",
        stage3_acceptance=args.acceptance,
        recipes=args.recipes,
        state_dir=str(args.state_dir),
        env_file=args.env_file,
    ))


def _resume_command(task_id: str, args: Stage4Args) -> list[str]:
    return [
        args.binary, "resume", task_id,
        "--state-dir", str(args.state_dir),
        "--providers", args.providers,
        "--provider-id", PROVIDER_ID,
        "--profile", args.profile,
        "--acceptance", args.acceptance,
        "--recipes", args.recipes,
        "--recipe-policy", "test=allow",
        "--write-policy", "write_file=allow,apply_patch=allow",
        "--retry-policy", "off",
        "--log-level", "error",
    ]


def run(args: Stage4Args) -> int:
    if args.env_file != EXPECTED_ENV_FILE:
        print("env_file_is_expected_external_reference=false")
        print("task_id=unavailable")
        print("stage4_result=stopped_preflight")
        return 2
    checks, key = load_key_without_emitting(args.env_file)
    for name, value in checks.items():
        print(f"{name}={str(value).lower()}")
    if key is None:
        print("task_id=unavailable")
        print("stage4_result=stopped_preflight")
        return 2

    child = child_environment(key)
    os.environ.pop(AUTH_REF, None)
    key = None
    command = _stage4_run_command(args)
    process = None
    try:
        args.state_dir.mkdir(parents=True, exist_ok=False)
        process = subprocess.Popen(
            command,
            env=child,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        deadline = time.monotonic() + args.max_wait_seconds
        checkpoint = None
        while time.monotonic() < deadline:
            if process.poll() is not None:
                print(f"initial_run_exit_code={process.returncode}")
                print("task_id=unavailable")
                print("stage4_result=checkpoint_not_observed")
                return 1
            snapshot = _read_snapshot(args.state_dir, args.workspace, args.expected_fix)
            if snapshot and checkpoint_ready(snapshot):
                checkpoint = snapshot
                break
            time.sleep(0.02)
        if checkpoint is None:
            process.kill()
            process.wait(timeout=10)
            print("task_id=unavailable")
            print("stage4_result=checkpoint_timeout")
            return 1

        task_id = checkpoint["task_id"]
        checkpoint_path = args.state_dir / "stage4-interrupt.json"
        _write_snapshot(checkpoint_path, checkpoint)
        if process.poll() is None:
            os.kill(process.pid, signal.SIGKILL)
        process.wait(timeout=10)
        if process.returncode != -signal.SIGKILL:
            print(f"initial_run_exit_code={process.returncode}")
            print(f"task_id={task_id}")
            print("stage4_result=interrupt_not_confirmed")
            return 1

        inspect_result = subprocess.run(
            [args.binary, "inspect", task_id, "--state-dir", str(args.state_dir)],
            env={name: value for name, value in child.items() if name != AUTH_REF},
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            check=False,
            timeout=30,
        )
        if inspect_result.returncode != 0:
            print(f"task_id={task_id}")
            print(f"inspect_exit_code={inspect_result.returncode}")
            print("stage4_result=inspect_failed")
            return 1

        resume_result = subprocess.run(
            _resume_command(task_id, args),
            env=child,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            check=False,
            timeout=args.max_wait_seconds,
        )
        final_snapshot = _read_snapshot(args.state_dir, args.workspace, args.expected_fix)
        if final_snapshot is None:
            print(f"task_id={task_id}")
            print(f"resume_exit_code={resume_result.returncode}")
            print("stage4_result=post_resume_state_missing")
            return 1
        _write_snapshot(args.state_dir / "stage4-resumed.json", final_snapshot)
        _write_snapshot(args.state_dir / "stage4-control.json", {
            "task_id": task_id,
            "interruption_signal": "SIGKILL",
            "initial_run_exit_code": process.returncode,
            "inspect_exit_code": inspect_result.returncode,
            "resume_exit_code": resume_result.returncode,
        })
        print(f"task_id={task_id}")
        print("interruption_signal=SIGKILL")
        print("interrupted_after_successful_recipe=true")
        print("interrupted_before_verification=true")
        print(f"inspect_exit_code={inspect_result.returncode}")
        print(f"resume_exit_code={resume_result.returncode}")
        print(f"resume_count={final_snapshot['resume_count']}")
        print(f"final_status={final_snapshot['task_status']}")
        print(f"provider_attempts={final_snapshot['provider_attempt_count']}")
        print(f"governor_admissions={final_snapshot['governor_admissions']}")
        print(f"attempt_debits={final_snapshot['attempt_debits']}")
        return resume_result.returncode
    except Exception as exc:
        print(f"runner_error_type={type(exc).__name__}")
        if process is not None and process.poll() is None:
            process.kill()
            process.wait(timeout=10)
        return 2
    finally:
        child.pop(AUTH_REF, None)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--bin", dest="binary", required=True)
    parser.add_argument("--workspace", required=True, type=Path)
    parser.add_argument("--providers", required=True)
    parser.add_argument("--profile", required=True)
    parser.add_argument("--acceptance", required=True)
    parser.add_argument("--recipes", required=True)
    parser.add_argument("--state-dir", required=True, type=Path)
    parser.add_argument("--env-file", required=True, type=Path)
    parser.add_argument("--expected-fix", required=True, type=Path)
    parser.add_argument("--max-wait-seconds", type=int, default=620)
    return run(Stage4Args(**vars(parser.parse_args())))


if __name__ == "__main__":
    raise SystemExit(main())
