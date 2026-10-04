#!/usr/bin/env python3
"""Read-only Stage 3 auditor; prints only bounded structured evidence."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import sqlite3
import subprocess
from pathlib import Path
from typing import Any

from stage1_models import load_key_into_process
from stage2_audit import _scan_secret, _safe

OBJECTIVE = (
    "Fix the whitespace handling bug in app/calc.go so the test suite passes.\n"
    "Inspect the relevant code and tests, make the minimum correct scoped change,\n"
    "run the declared test recipe, and finish only after Runstead's independent\n"
    "acceptance verification passes."
)
PROVIDER_ID = "groq-gpt-oss-120b-canary-v4"
MODEL = "openai/gpt-oss-120b"
FAMILY = "openai_compatible"
EVIDENCE_TYPES = {"read_file", "list_files", "search_text", "git_status", "git_diff", "write_file", "apply_patch", "run_recipe"}


def _object(raw: str, label: str) -> dict[str, Any]:
    value = json.loads(raw)
    if not isinstance(value, dict):
        raise ValueError(f"{label}_not_object")
    return value


def scoped(path: str) -> bool:
    return path == "app/calc.go"


def recipe_passed(data_json: str) -> bool:
    data = _object(data_json, "recipe_result")
    return (data.get("recipe_id") == "test" and data.get("started") is True
            and data.get("exit_code") == 0 and data.get("timed_out") is False
            and data.get("canceled") is False and data.get("signal", "") == ""
            and data.get("stdout_truncated") is False and data.get("stderr_truncated") is False)


def audit(state_dir: Path, task_id: str, workspace: Path, acceptance_path: Path,
          expected_fix: Path, auth_env_file: Path) -> list[str]:
    errors: list[str] = []
    conn = sqlite3.connect((state_dir / "runstead.db").as_uri() + "?mode=ro", uri=True)
    conn.row_factory = sqlite3.Row
    conn.execute("PRAGMA query_only=ON")
    if conn.execute("PRAGMA query_only").fetchone()[0] != 1:
        raise ValueError("query_only_unavailable")
    task = conn.execute(
        "SELECT task_id,objective,status,outcome,workspace,model,config_json,execution_contract_json,execution_contract_hash "
        "FROM tasks WHERE task_id=?", (task_id,)
    ).fetchone()
    if task is None:
        raise ValueError("task_missing")
    if task["objective"] != OBJECTIVE:
        errors.append("objective_mismatch")
    if Path(task["workspace"]).resolve() != workspace.resolve():
        errors.append("workspace_mismatch")
    if task["model"] != MODEL:
        errors.append("model_mismatch")
    config = _object(task["config_json"], "task_config")
    if config.get("provider_id") != PROVIDER_ID or config.get("protocol_family") != FAMILY or config.get("provider_model") != MODEL:
        errors.append("provider_config_mismatch")
    contract_raw = task["execution_contract_json"]
    contract = _object(contract_raw, "execution_contract")
    provider = contract.get("provider")
    if not isinstance(provider, dict) or provider.get("provider_id") != PROVIDER_ID or provider.get("protocol_family") != FAMILY or provider.get("model") != MODEL:
        errors.append("provider_contract_mismatch")
    if task["execution_contract_hash"] != "sha256:" + hashlib.sha256(contract_raw.encode()).hexdigest():
        errors.append("execution_contract_hash_mismatch")

    acceptance_saved = json.loads(acceptance_path.read_text(encoding="utf-8"))
    persisted = conn.execute("SELECT spec_json,digest FROM acceptance_plans WHERE task_id=?", (task_id,)).fetchall()
    if len(persisted) != 1 or _object(persisted[0]["spec_json"], "acceptance") != acceptance_saved:
        errors.append("acceptance_mismatch")
        acceptance_digest = "unknown"
    else:
        acceptance_digest = persisted[0]["digest"]
    events = conn.execute("SELECT sequence,kind,payload_json FROM events WHERE task_id=? ORDER BY sequence", (task_id,)).fetchall()
    saves = [row for row in events if row["kind"] == "acceptance_plan_saved"]
    prepared = min((row["sequence"] for row in events if row["kind"] == "provider_attempt_prepared"), default=None)
    predispatch = len(saves) == 1 and prepared is not None and saves[0]["sequence"] < prepared
    if not predispatch:
        errors.append("acceptance_not_frozen_before_dispatch")

    provider_attempts = conn.execute(
        "SELECT execution_id,attempt_sequence,status,outcome,delivery_state,provider_failure_class,error_class,attempt_debited,provider,model,protocol_family "
        "FROM provider_attempts WHERE task_id=? ORDER BY attempt_sequence", (task_id,)
    ).fetchall()
    ledger = conn.execute("SELECT COUNT(*) FROM governor_ledger WHERE task_id=?", (task_id,)).fetchone()[0]
    governor = conn.execute("SELECT attempts,retries FROM governor_task_states WHERE task_id=?", (task_id,)).fetchone()
    debit_count = sum(row["attempt_debited"] for row in provider_attempts)
    accounting_ok = ledger == len(provider_attempts) == debit_count and governor is not None and governor["attempts"] == len(provider_attempts) and governor["retries"] == 0
    if not accounting_ok:
        errors.append("attempt_admission_debit_mismatch")
    if any(row["provider"] != PROVIDER_ID or row["model"] != MODEL or row["protocol_family"] != FAMILY for row in provider_attempts):
        errors.append("provider_attempt_identity_mismatch")

    action_rows = conn.execute("SELECT action_id,tool,arguments_json FROM actions WHERE task_id=? ORDER BY action_sequence", (task_id,)).fetchall()
    attempts = conn.execute("SELECT execution_id,action_id,tool,status,arguments_json,evidence_id,effect_after_hash,created_at FROM tool_attempts WHERE task_id=? ORDER BY created_at,execution_id", (task_id,)).fetchall()
    results = conn.execute(
        "SELECT r.evidence_id,r.success,r.data_json,r.metadata_json,r.created_at,t.execution_id,t.tool,t.arguments_json,t.status,t.effect_after_hash "
        "FROM tool_results r JOIN tool_attempts t ON t.execution_id=r.execution_id WHERE r.task_id=? ORDER BY r.created_at,r.evidence_id", (task_id,)
    ).fetchall()
    reads: list[tuple[str, str, str]] = []
    writes: list[dict[str, str]] = []
    recipe_rows: list[sqlite3.Row] = []
    for row in results:
        if not row["success"]:
            continue
        data = _object(row["data_json"], "tool_result")
        meta = _object(row["metadata_json"], "tool_metadata")
        if row["tool"] == "read_file" and data.get("path") and data.get("sha256"):
            reads.append((data["path"], data["sha256"], row["created_at"]))
        elif row["tool"] in {"write_file", "apply_patch"}:
            args = _object(row["arguments_json"], "write_arguments")
            path = data.get("path", meta.get("path", ""))
            entry = {"execution_id": row["execution_id"], "tool": row["tool"], "path": path,
                     "before_hash": data.get("before_hash", ""), "after_hash": data.get("after_hash", ""),
                     "expected_before_hash": args.get("expected_before_hash", ""),
                     "effect_after_hash": row["effect_after_hash"]}
            writes.append(entry)
        elif row["tool"] == "run_recipe":
            recipe_rows.append(row)

    inspected = {path for path, _digest, _at in reads}
    if not {"app/calc.go", "app/calc_test.go"}.issubset(inspected):
        errors.append("required_code_or_test_inspection_missing")
    attempted_writes = [row for row in attempts if row["tool"] in {"write_file", "apply_patch"}]
    if not writes or not attempted_writes:
        errors.append("scoped_write_missing")
    for attempt in attempted_writes:
        args = _object(attempt["arguments_json"], "write_arguments")
        if not scoped(args.get("path", "")):
            errors.append("write_attempt_out_of_scope")
            break
    if any(not scoped(item["path"]) or not item["before_hash"] or not item["after_hash"]
           or item["expected_before_hash"] != item["before_hash"]
           or item["effect_after_hash"] != item["after_hash"] for item in writes):
        errors.append("write_scope_or_hash_reconciliation_failed")
    for item in writes:
        attempt_time = next((row["created_at"] for row in attempts if row["execution_id"] == item["execution_id"]), "")
        if not any(path == item["path"] and digest == item["before_hash"] and read_time <= attempt_time
                   for path, digest, read_time in reads):
            errors.append("stale_state_read_hash_missing")
            break
    for row in action_rows:
        if row["tool"] in {"write_file", "apply_patch"}:
            args = _object(row["arguments_json"], "write_action_arguments")
            if not scoped(args.get("path", "")):
                errors.append("write_action_out_of_scope")
                break

    expected_hash = hashlib.sha256(expected_fix.read_bytes()).hexdigest()
    actual_hash = hashlib.sha256((workspace / "app/calc.go").read_bytes()).hexdigest()
    if actual_hash != expected_hash:
        errors.append("final_source_differs_from_expected_fix")
    git_names = subprocess.run(["git", "-C", str(workspace), "diff", "--name-only"],
                               check=True, capture_output=True, text=True).stdout.splitlines()
    untracked = subprocess.run(["git", "-C", str(workspace), "ls-files", "--others", "--exclude-standard"],
                               check=True, capture_output=True, text=True).stdout.splitlines()
    changed = sorted(set(git_names + untracked))
    if changed != ["app/calc.go"]:
        errors.append("workspace_change_scope_failed")

    for action in action_rows:
        if action["tool"] == "run_recipe":
            args = _object(action["arguments_json"], "recipe_action_arguments")
            if args.get("recipe_id") != "test":
                errors.append("undeclared_recipe_action")
                break
    recipe_passes = [row for row in recipe_rows if recipe_passed(row["data_json"])]
    if not recipe_passes:
        errors.append("declared_test_recipe_never_passed")
    if any(_object(row["data_json"], "recipe_result").get("recipe_id") != "test" for row in recipe_rows):
        errors.append("undeclared_recipe_observed")

    verifications = conn.execute("SELECT sequence,decision FROM verification_attempts WHERE task_id=? ORDER BY sequence", (task_id,)).fetchall()
    if not verifications or verifications[-1]["decision"] != "passed":
        errors.append("independent_verifier_not_passed")
    elif len(verifications):
        checks = conn.execute("SELECT check_id,status FROM verification_checks WHERE task_id=? AND attempt_id=(SELECT attempt_id FROM verification_attempts WHERE task_id=? ORDER BY sequence DESC LIMIT 1)", (task_id, task_id)).fetchall()
        if {row["check_id"]: row["status"] for row in checks}.get("tests-pass") != "passed" or {row["check_id"]: row["status"] for row in checks}.get("calc-correct-fix") != "passed":
            errors.append("acceptance_checks_not_passed")
    if task["status"] != "completed" or task["outcome"] != "completed":
        errors.append("task_not_terminal_completed")

    try:
        key, _ = load_key_into_process()
        _scan_secret(state_dir, key)
        secret_scan = "clean"
    except Exception:
        secret_scan = "unavailable/LIMITED"

    print("audit_result=" + ("PASS" if not errors else "NOT_QUALIFIED"))
    print(f"task_id={task_id} status={_safe(task['status'], {'planned','running','completed','failed','canceled','human_review_required'})} outcome={_safe(task['outcome'], {'completed','provider_failure','verification_failures_exhausted','final_not_grounded','verification_blocked','tool_failure','failed','canceled'})}")
    print(f"provider_attempts={len(provider_attempts)} governor_admissions={ledger} attempt_debits={debit_count} retries={governor['retries'] if governor else 'unknown'}")
    for row in provider_attempts:
        failure = "empty" if not row["provider_failure_class"] else _safe(row["provider_failure_class"], {
            "config_refused","auth_unavailable","authentication_denied","permission_denied","rate_or_capacity",
            "timeout","cancelled","malformed_response","invalid_envelope","empty_response","unsupported_response_format",
            "incomplete_completion","refusal","response_too_large","request_too_large","upstream_server_failure",
            "upstream_http_failure","unsafe_redirect","transport"})
        receipt = "empty" if not row["error_class"] else _safe(row["error_class"], {"attempt_receipts_missing","attempt_receipts_invalid"})
        print(f"attempt={row['execution_id']} seq={row['attempt_sequence']} status={_safe(row['status'], {'planned','prepared','running','completed','failed','uncertain','reconciled','canceled','human_review_required'})} outcome={_safe(row['outcome'], {'success','rate_or_capacity','authentication_expired','authentication_denied','http_403','login_challenge','captcha','suspicious_activity','account_warning','feature_restriction','connection_reset','timeout','empty_response','malformed_upstream_response','upstream_server_failure','cancelled_before_upstream','uncertain_reached'})} delivery_state={_safe(row['delivery_state'], {'unobserved','not_sent','sent_confirmed','sent_unconfirmed','completed'})} provider_failure_class={failure} receipt_error={receipt} debited={row['attempt_debited']}")
    print("read_paths=" + (",".join(sorted(inspected)) if inspected else "none"))
    print("writes=" + (",".join(f"{x['execution_id']}:{x['path']}:{x['before_hash']}->{x['after_hash']}" for x in writes) or "none"))
    print(f"final_calc_hash={actual_hash} expected_fix_hash={expected_hash} changed_paths={','.join(changed) if changed else 'none'}")
    recipe_exits = []
    for row in recipe_rows:
        recipe = _object(row["data_json"], "recipe_result")
        recipe_exits.append(str(recipe.get("exit_code", "unknown")))
    print(f"recipe_attempts={len(recipe_rows)} recipe_exit_codes={','.join(recipe_exits) if recipe_exits else 'none'} recipe_pass={bool(recipe_passes)}")
    print(f"verification_attempts={len(verifications)} latest_verifier_pass={bool(verifications and verifications[-1]['decision']=='passed')} acceptance_saved_before_dispatch={predispatch} acceptance_digest={acceptance_digest}")
    print(f"secret_scan={secret_scan}")
    print("failed_checks=" + (",".join(errors) if errors else "none"))
    conn.close()
    return errors


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--state-dir", required=True, type=Path)
    parser.add_argument("--task-id", required=True)
    parser.add_argument("--workspace", required=True, type=Path)
    parser.add_argument("--acceptance", required=True, type=Path)
    parser.add_argument("--expected-fix", required=True, type=Path)
    parser.add_argument("--env-file", required=True, type=Path)
    args = parser.parse_args()
    os.environ["RUNSTEAD_GROQ_ENV_FILE"] = str(args.env_file)
    try:
        errors = audit(args.state_dir, args.task_id, args.workspace, args.acceptance, args.expected_fix, args.env_file)
    except Exception as exc:
        print(f"audit_result=LIMITED error_type={type(exc).__name__}")
        return 2
    return 0 if not errors else 1


if __name__ == "__main__":
    raise SystemExit(main())
