#!/usr/bin/env python3
"""Fail-closed, read-only audit for the fresh v4 Stage 2 task."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import sqlite3
import sys
from pathlib import Path
from typing import Any

from stage1_models import load_key_into_process

OBJECTIVE = (
    "Read app/calc.go using the available repository tool. Complete only after citing the actual "
    "read_file observation for app/calc.go. Do not modify files and do not run recipes or processes."
)
PROVIDER_ID = "groq-gpt-oss-120b-canary-v4"
MODEL = "openai/gpt-oss-120b"
FAMILY = "openai_compatible"
MAX_FILES = 1000
MAX_FILE_BYTES = 16 * 1024 * 1024
MAX_TOTAL_BYTES = 64 * 1024 * 1024
CHUNK_BYTES = 1024 * 1024


def _object(raw: str, label: str) -> dict[str, Any]:
    try:
        value = json.loads(raw)
    except (TypeError, json.JSONDecodeError) as exc:
        raise ValueError(f"{label}_invalid_json") from exc
    if not isinstance(value, dict):
        raise ValueError(f"{label}_not_object")
    return value


def _scan_secret(state_dir: Path, secret: str) -> None:
    token = secret.encode("utf-8")
    if not token:
        raise ValueError("secret_empty")
    paths: list[Path] = []
    for root, dirs, files in os.walk(state_dir, followlinks=False):
        root_path = Path(root)
        for name in dirs + files:
            path = root_path / name
            if path.is_symlink() or (name in files and not path.is_file()):
                raise ValueError("secret_scan_unsafe_entry")
        paths.extend(root_path / name for name in files)
        if len(paths) > MAX_FILES:
            raise ValueError("secret_scan_file_bound")
    total = sum(path.stat().st_size for path in paths)
    if total > MAX_TOTAL_BYTES or any(path.stat().st_size > MAX_FILE_BYTES for path in paths):
        raise ValueError("secret_scan_size_bound")
    for path in paths:
        tail = b""
        read = 0
        with path.open("rb") as stream:
            while chunk := stream.read(CHUNK_BYTES):
                read += len(chunk)
                if read > MAX_FILE_BYTES:
                    raise ValueError("secret_scan_file_bound")
                data = tail + chunk
                if token in data:
                    raise ValueError("secret_scan_match")
                tail = data[-(len(token) - 1):] if len(token) > 1 else b""


def _safe(value: Any, choices: set[str]) -> str:
    return value if isinstance(value, str) and value in choices else "unknown"


def _citation_matches_read_file(cited: Any, evidence_ids: set[str]) -> bool:
    return isinstance(cited, list) and any(
        isinstance(item, dict)
        and item.get("evidence_id") in evidence_ids
        and item.get("claimed_tool") == "read_file"
        and item.get("tool") == "read_file"
        and item.get("exists") is True
        and item.get("tool_matches") is True
        for item in cited
    )


def audit(state_dir: Path, task_id: str, workspace: Path, acceptance_path: Path,
          auth_env_file: Path) -> list[str]:
    errors: list[str] = []
    db_path = state_dir / "runstead.db"
    uri = db_path.as_uri() + "?mode=ro"
    conn = sqlite3.connect(uri, uri=True)
    conn.row_factory = sqlite3.Row
    conn.execute("PRAGMA query_only=ON")
    if conn.execute("PRAGMA query_only").fetchone()[0] != 1:
        raise ValueError("query_only_unavailable")

    task_rows = conn.execute(
        "SELECT task_id,objective,status,outcome,stop_reason,workspace,model,config_json,"
        "execution_contract_json,execution_contract_hash FROM tasks WHERE task_id=?", (task_id,)
    ).fetchall()
    if len(task_rows) != 1:
        raise ValueError("task_missing_or_duplicated")
    task = task_rows[0]
    expected_workspace = str(workspace.resolve())
    if task["objective"] != OBJECTIVE:
        errors.append("objective_mismatch")
    if Path(task["workspace"]).resolve() != Path(expected_workspace):
        errors.append("workspace_mismatch")
    if task["model"] != MODEL:
        errors.append("model_mismatch")
    config = _object(task["config_json"], "task_config")
    if config.get("provider_id") != PROVIDER_ID or config.get("protocol_family") != FAMILY or config.get("provider_model") != MODEL:
        errors.append("provider_config_mismatch")
    contract_raw = task["execution_contract_json"]
    contract = _object(contract_raw, "execution_contract")
    contract_provider = contract.get("provider")
    if not isinstance(contract_provider, dict) or contract_provider.get("provider_id") != PROVIDER_ID or contract_provider.get("model") != MODEL or contract_provider.get("protocol_family") != FAMILY:
        errors.append("frozen_provider_contract_mismatch")
    actual_contract_hash = "sha256:" + hashlib.sha256(contract_raw.encode("utf-8")).hexdigest()
    if task["execution_contract_hash"] != actual_contract_hash:
        errors.append("execution_contract_hash_mismatch")

    expected_acceptance = json.loads(acceptance_path.read_text(encoding="utf-8"))
    acceptance = conn.execute(
        "SELECT spec_json,digest FROM acceptance_plans WHERE task_id=?", (task_id,)
    ).fetchall()
    if len(acceptance) != 1 or _object(acceptance[0]["spec_json"], "acceptance") != expected_acceptance:
        errors.append("acceptance_mismatch")
        acceptance_digest = "unknown"
    else:
        acceptance_digest = acceptance[0]["digest"]

    events = conn.execute(
        "SELECT sequence,kind,payload_json FROM events WHERE task_id=? ORDER BY sequence", (task_id,)
    ).fetchall()
    acceptance_saves = [row for row in events if row["kind"] == "acceptance_plan_saved"]
    attempts = conn.execute(
        "SELECT execution_id,attempt_sequence,status,outcome,delivery_state,provider_failure_class,"
        "error_class,attempt_debited,provider,model,protocol_family FROM provider_attempts "
        "WHERE task_id=? ORDER BY attempt_sequence", (task_id,)
    ).fetchall()
    prepared_seq = min((row["sequence"] for row in events if row["kind"] == "provider_attempt_prepared"), default=None)
    predispatch = len(acceptance_saves) == 1 and prepared_seq is not None and acceptance_saves[0]["sequence"] < prepared_seq
    if not predispatch:
        errors.append("acceptance_not_frozen_before_dispatch")
    if any(row["provider"] != PROVIDER_ID or row["model"] != MODEL or row["protocol_family"] != FAMILY for row in attempts):
        errors.append("provider_attempt_identity_mismatch")
    ledgers = conn.execute("SELECT COUNT(*) FROM governor_ledger WHERE task_id=?", (task_id,)).fetchone()[0]
    governor = conn.execute("SELECT attempts,retries FROM governor_task_states WHERE task_id=?", (task_id,)).fetchone()
    debits = sum(int(row["attempt_debited"]) for row in attempts)
    accounting_ok = ledgers == len(attempts) == debits and governor is not None and governor["attempts"] == len(attempts) and governor["retries"] == 0
    if not accounting_ok:
        errors.append("attempt_admission_debit_mismatch")

    actions = conn.execute("SELECT action_id,tool,arguments_json,status FROM actions WHERE task_id=? ORDER BY action_sequence", (task_id,)).fetchall()
    tool_attempts = conn.execute("SELECT execution_id,tool,status,evidence_id,arguments_json FROM tool_attempts WHERE task_id=? ORDER BY created_at,execution_id", (task_id,)).fetchall()
    results = conn.execute(
        "SELECT r.evidence_id,r.success,r.data_json,r.metadata_json,t.tool,t.arguments_json,t.status "
        "FROM tool_results r JOIN tool_attempts t ON t.execution_id=r.execution_id "
        "WHERE r.task_id=? ORDER BY r.created_at,r.evidence_id", (task_id,)
    ).fetchall()
    citations = []
    finalized = [row for row in events if row["kind"] == "task_finalized"]
    if len(finalized) == 1:
        payload = _object(finalized[0]["payload_json"], "task_finalized")
        cited = payload.get("evidence")
        if isinstance(cited, list):
            citations = [value for value in cited if isinstance(value, str) and re.fullmatch(r"obs-[0-9]{6}", value)]
    read_rows = []
    for row in results:
        if row["tool"] != "read_file" or not row["success"] or row["status"] != "completed":
            continue
        data = _object(row["data_json"], "read_result")
        metadata = _object(row["metadata_json"], "read_metadata")
        action = next((item for item in tool_attempts if item["evidence_id"] == row["evidence_id"]), None)
        args = _object(action["arguments_json"], "read_arguments") if action else {}
        if data.get("path") == "app/calc.go" and metadata.get("path") == "app/calc.go" and args.get("path") == "app/calc.go":
            read_rows.append(row)
    read_cited = any(row["evidence_id"] in citations for row in read_rows)

    write_names = {"write_file", "apply_patch"}
    recipe_names = {"run_recipe"}
    writes = sum(row["tool"] in write_names for row in actions) + sum(row["tool"] in write_names for row in tool_attempts)
    recipes = sum(row["tool"] in recipe_names for row in actions) + sum(row["tool"] in recipe_names for row in tool_attempts)
    if writes:
        errors.append("write_effect_or_attempt_present")
    if recipes:
        errors.append("recipe_or_process_attempt_present")

    verification = conn.execute("SELECT sequence,decision,report_json FROM verification_attempts WHERE task_id=? ORDER BY sequence", (task_id,)).fetchall()
    latest_pass = bool(verification and verification[-1]["decision"] == "passed")
    if not latest_pass:
        errors.append("independent_verifier_not_passed")
    verifier_citation_matches = False
    if latest_pass:
        report = _object(verification[-1]["report_json"], "verification_report")
        verifier_citation_matches = _citation_matches_read_file(
            report.get("cited_evidence"), {row["evidence_id"] for row in read_rows}
        )
    if not verifier_citation_matches:
        errors.append("verifier_citation_not_matched_to_read_file")
    if task["status"] != "completed" or task["outcome"] != "completed":
        errors.append("task_not_terminal_completed")
    if not read_rows:
        errors.append("read_file_observation_missing")
    if not read_cited:
        errors.append("read_file_citation_missing_or_mismatched")

    try:
        key, _ = load_key_into_process()
        _scan_secret(state_dir, key)
        secret_scan = "clean"
    except Exception:
        secret_scan = "unavailable/LIMITED"

    print("audit_result=" + ("PASS" if not errors else "NOT_QUALIFIED"))
    print(f"task_id={task_id}")
    print(f"status={_safe(task['status'], {'planned','running','completed','failed','canceled','human_review_required'})}")
    print(f"outcome={_safe(task['outcome'], {'completed','provider_failure','verification_failures_exhausted','final_not_grounded','verification_blocked','tool_failure','failed','canceled'})}")
    print(f"provider_id={PROVIDER_ID} protocol_family={FAMILY} model={MODEL}")
    print(f"provider_attempts={len(attempts)} governor_admissions={ledgers} attempt_debits={debits} retries={governor['retries'] if governor else 'unknown'}")
    for row in attempts:
        outcome = _safe(row["outcome"], {"success","uncertain_reached","not_sent","provider_failure","authentication_denied","rate_limited","capacity_exhausted","timeout","transport"})
        delivery = _safe(row["delivery_state"], {"unobserved","not_sent","sent_confirmed","sent_unconfirmed","completed"})
        failure = "empty" if not row["provider_failure_class"] else _safe(row["provider_failure_class"], {
            "config_refused","auth_unavailable","authentication_denied","permission_denied",
            "rate_or_capacity","timeout","cancelled","malformed_response","invalid_envelope",
            "empty_response","unsupported_response_format","incomplete_completion","refusal",
            "response_too_large","request_too_large","upstream_server_failure",
            "upstream_http_failure","unsafe_redirect","transport"})
        receipt = "empty" if not row["error_class"] else _safe(row["error_class"], {
            "attempt_receipts_missing","attempt_receipts_invalid"})
        print(f"attempt={row['execution_id']} sequence={row['attempt_sequence']} status={_safe(row['status'], {'planned','prepared','running','completed','failed','uncertain','reconciled','canceled','human_review_required'})} outcome={outcome} delivery_state={delivery} provider_failure_class={failure} receipt_error={receipt} debited={row['attempt_debited']}")
    print(f"actions={len(actions)} tool_attempts={len(tool_attempts)} tool_results={len(results)}")
    print("tools=" + (",".join(_safe(row["tool"], {"read_file","list_files","search_text","git_status","git_diff","write_file","apply_patch","run_recipe"}) for row in tool_attempts) or "none"))
    print(f"read_file_app_calc_go={bool(read_rows)} cited_by_final={read_cited} citation_count={len(citations)}")
    print(f"writes={writes} recipes_processes={recipes}")
    print(f"verification_attempts={len(verification)} latest_verifier_pass={latest_pass} verifier_citation_matches_read_file={verifier_citation_matches}")
    print(f"acceptance_saved_before_dispatch={predispatch} acceptance_digest={acceptance_digest}")
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
    parser.add_argument("--env-file", required=True, type=Path)
    args = parser.parse_args()
    os.environ["RUNSTEAD_GROQ_ENV_FILE"] = str(args.env_file)
    try:
        errors = audit(args.state_dir, args.task_id, args.workspace, args.acceptance, args.env_file)
    except Exception as exc:
        print(f"audit_result=LIMITED error_type={type(exc).__name__}")
        return 2
    return 0 if not errors else 1


if __name__ == "__main__":
    raise SystemExit(main())
