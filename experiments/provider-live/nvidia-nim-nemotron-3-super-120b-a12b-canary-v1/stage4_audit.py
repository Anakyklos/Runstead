#!/usr/bin/env python3
"""Read-only Stage 4 audit for same-task durable interruption and resume."""

from __future__ import annotations

import argparse
import json
import os
import sqlite3
from pathlib import Path
from typing import Any

from stage1_control import AUTH_REF, load_key_without_emitting
from stage2_audit import _scan_secret
from stage4_runner import _read_snapshot


def _effect_ids(snapshot: dict[str, Any]) -> list[str]:
    return [
        row["execution_id"]
        for row in snapshot.get("tool_attempts", [])
        if row.get("tool") in {"write_file", "apply_patch", "run_recipe"}
    ]


def reconcile_snapshots(
    before: dict[str, Any], after: dict[str, Any], control: dict[str, Any]
) -> list[str]:
    errors: list[str] = []
    if control.get("task_id") != before.get("task_id") or before.get("task_id") != after.get("task_id"):
        errors.append("task_id_changed")
    if control.get("interruption_signal") != "SIGKILL" or control.get("inspect_exit_code") != 0:
        errors.append("inspect_or_interruption_not_proven")
    if control.get("resume_exit_code") != 0:
        errors.append("resume_command_failed")
    for field in ("objective_matches", "workspace_matches", "model_matches",
                  "provider_id_matches", "provider_model_matches", "provider_protocol_matches"):
        if before.get(field) is not True or after.get(field) is not True:
            errors.append("frozen_task_identity_mismatch")
            break
    for field in ("config_sha256", "execution_contract_hash", "acceptance_digest"):
        if not before.get(field) or before.get(field) != after.get(field):
            errors.append("frozen_config_or_acceptance_changed")
            break
    if before.get("task_status") != "running" or before.get("resume_count") != 0:
        errors.append("interrupted_checkpoint_not_running")
    if before.get("verification_attempts") != 0:
        errors.append("interrupted_after_verification_started")
    if before.get("target_write_durable") is not True or before.get("target_hash_matches") is not True:
        errors.append("durable_scoped_write_missing_at_interruption")
    if before.get("successful_test_recipe_durable") is not True:
        errors.append("successful_recipe_missing_at_interruption")
    if before.get("unsettled_provider_attempts") != 0:
        errors.append("uncertain_delivery_reached_checkpoint")
    if _effect_ids(before) != _effect_ids(after):
        errors.append("completed_effect_replayed")
    if not set(before.get("evidence_ids", [])).issubset(set(after.get("evidence_ids", []))):
        errors.append("durable_evidence_lost_on_resume")
    if after.get("resume_count", 0) < 1:
        errors.append("resume_count_not_incremented")
    if after.get("task_status") != "completed" or after.get("latest_verification") != "passed":
        errors.append("resumed_task_or_verifier_not_complete")
    if after.get("target_hash_matches") is not True:
        errors.append("final_expected_hash_missing")
    if after.get("governor_retries") != 0:
        errors.append("automatic_retry_present")

    prior = before.get("provider_attempts", [])
    final = after.get("provider_attempts", [])
    final_by_id = {row.get("execution_id"): row for row in final}
    if len(final_by_id) != len(final):
        errors.append("provider_attempt_identity_duplicated")
    for row in prior:
        if final_by_id.get(row.get("execution_id")) != row:
            errors.append("prior_provider_attempt_changed_or_missing")
            break
    if any(
        row.get("uncertain") or row.get("delivery_state") == "sent_unconfirmed"
        for row in prior
    ):
        errors.append("uncertain_delivery_reached_checkpoint")
    if len({row.get("client_request_id") for row in final}) != len(final):
        errors.append("client_request_replayed")
    if prior and any(
        row.get("attempt_sequence", 0) <= max(item.get("attempt_sequence", 0) for item in prior)
        for row in final[len(prior):]
    ):
        errors.append("provider_attempt_sequence_reset")
    for snapshot in (before, after):
        if snapshot.get("provider_attempt_count") != snapshot.get("governor_admissions"):
            errors.append("attempt_admission_mismatch")
            break
        if snapshot.get("provider_attempt_count") != snapshot.get("attempt_debits"):
            errors.append("attempt_debit_mismatch")
            break
        if snapshot.get("provider_attempt_count") != snapshot.get("governor_attempts"):
            errors.append("governor_attempt_count_mismatch")
            break
        if snapshot.get("governor_retries") != 0:
            errors.append("automatic_retry_present")
            break
    return sorted(set(errors))


def audit(state_dir: Path, task_id: str, workspace: Path, expected_fix: Path,
          auth_env_file: Path) -> list[str]:
    errors: list[str] = []
    before_path = state_dir / "stage4-interrupt.json"
    after_path = state_dir / "stage4-resumed.json"
    control_path = state_dir / "stage4-control.json"
    try:
        before = json.loads(before_path.read_text(encoding="utf-8"))
        after = json.loads(after_path.read_text(encoding="utf-8"))
        control = json.loads(control_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        raise ValueError("stage4_control_evidence_missing_or_invalid")
    errors.extend(reconcile_snapshots(before, after, control))
    if before.get("task_id") != task_id or after.get("task_id") != task_id:
        errors.append("requested_task_id_mismatch")

    current = _read_snapshot(state_dir, workspace, expected_fix)
    if current is None or current != after:
        errors.append("post_resume_snapshot_does_not_match_live_sqlite")
    if current is not None:
        conn = sqlite3.connect((state_dir / "runstead.db").as_uri() + "?mode=ro", uri=True)
        conn.row_factory = sqlite3.Row
        try:
            conn.execute("PRAGMA query_only=ON")
            events = {
                row["kind"] for row in conn.execute(
                    "SELECT kind FROM events WHERE task_id=?", (task_id,)
                ).fetchall()
            }
            if not {"recovery_started", "recovery_context_reconstructed", "recovery_continued"}.issubset(events):
                errors.append("sqlite_recovery_reconstruction_not_proven")
        finally:
            conn.close()

    try:
        _checks, key = load_key_without_emitting(auth_env_file)
        if key is None:
            raise ValueError("external_key_unavailable")
        _scan_secret(state_dir, key)
        secret_scan = "clean"
    except Exception:
        secret_scan = "unavailable/LIMITED"
        errors.append("secret_scan_unavailable")
    finally:
        os.environ.pop(AUTH_REF, None)

    print("audit_result=" + ("PASS" if not errors else "NOT_QUALIFIED"))
    print(f"task_id={task_id}")
    print(f"task_ids_same={before.get('task_id') == after.get('task_id') == task_id}")
    print(f"resume_count={after.get('resume_count', 'unknown')}")
    print(f"initial_provider_attempts={before.get('provider_attempt_count', 'unknown')} final_provider_attempts={after.get('provider_attempt_count', 'unknown')}")
    print(f"initial_admissions={before.get('governor_admissions', 'unknown')} final_admissions={after.get('governor_admissions', 'unknown')}")
    print(f"initial_debits={before.get('attempt_debits', 'unknown')} final_debits={after.get('attempt_debits', 'unknown')}")
    print(f"initial_writes={before.get('successful_target_writes', 'unknown')} final_writes={after.get('successful_target_writes', 'unknown')}")
    print(f"initial_test_recipes={before.get('successful_test_recipe_durable')} final_test_recipes={after.get('successful_test_recipe_durable')}")
    print(f"verification_before_interrupt={before.get('verification_attempts', 'unknown')} final_verifier={after.get('latest_verification', 'unknown')}")
    print(f"interrupt_signal={control.get('interruption_signal', 'unknown')} inspect_exit_code={control.get('inspect_exit_code', 'unknown')} resume_exit_code={control.get('resume_exit_code', 'unknown')}")
    print("uncertain_delivery_at_checkpoint=none" if before.get("unsettled_provider_attempts") == 0 else "uncertain_delivery_at_checkpoint=present")
    print(f"secret_scan={secret_scan}")
    print("failed_checks=" + (",".join(sorted(set(errors))) if errors else "none"))
    return sorted(set(errors))


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--state-dir", required=True, type=Path)
    parser.add_argument("--task-id", required=True)
    parser.add_argument("--workspace", required=True, type=Path)
    parser.add_argument("--expected-fix", required=True, type=Path)
    parser.add_argument("--env-file", required=True, type=Path)
    args = parser.parse_args()
    try:
        errors = audit(args.state_dir, args.task_id, args.workspace, args.expected_fix, args.env_file)
    except Exception as exc:
        print(f"audit_result=LIMITED error_type={type(exc).__name__}")
        return 2
    return 0 if not errors else 1


if __name__ == "__main__":
    raise SystemExit(main())
