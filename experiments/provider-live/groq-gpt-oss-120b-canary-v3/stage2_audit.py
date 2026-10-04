#!/usr/bin/env python3
"""Read-only, fail-closed audit of the retained Groq v3 Stage 2 SQLite state."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import sqlite3
import sys
from pathlib import Path
from typing import Any


TASK_ID = "cli-1791122946929726868"
OBJECTIVE = (
    "Read app/calc.go using the available repository tool. Complete only after citing the actual "
    "read_file observation for app/calc.go. Do not modify files and do not run recipes or processes."
)
PROVIDER_ID = "groq-gpt-oss-120b-canary-v3"
PROTOCOL_FAMILY = "openai_compatible"
MODEL = "openai/gpt-oss-120b"
ADAPTER_VERSION = "compatible-provider-v0.1"
EXPECTED_ACCEPTANCE_DIGEST = "3c1206c11be99bbd41f780830b899902ba3bb17c807d67081a30bfdd9e932af1"
MAX_STATE_FILES = 1000
MAX_STATE_TOTAL_BYTES = 64 * 1024 * 1024
MAX_STATE_FILE_BYTES = 16 * 1024 * 1024
SCAN_CHUNK_BYTES = 1024 * 1024


class AuditError(Exception):
    """An audit expectation diverged or safe inspection was not possible."""


def _require(condition: bool, message: str) -> None:
    if not condition:
        raise AuditError(message)


def _decode_object(raw: str, label: str) -> dict[str, Any]:
    def reject_duplicate_keys(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
        value: dict[str, Any] = {}
        for key, item in pairs:
            if key in value:
                raise AuditError(f"{label} contains a duplicate JSON key")
            value[key] = item
        return value

    try:
        value = json.loads(raw, object_pairs_hook=reject_duplicate_keys)
    except (TypeError, json.JSONDecodeError) as exc:
        raise AuditError(f"{label} is not valid JSON") from exc
    _require(isinstance(value, dict), f"{label} is not a JSON object")
    return value


def _acceptance_digest(spec_raw: str) -> str:
    spec = _decode_object(spec_raw, "acceptance plan")
    _require(type(spec.get("version")) is int and spec["version"] == 1,
             "acceptance plan version differs")
    checks = spec.get("checks")
    _require(isinstance(checks, list), "acceptance plan checks are missing")
    lines = [f"version={spec['version']}"]
    seen: set[str] = set()
    for check in checks:
        _require(isinstance(check, dict), "acceptance check is malformed")
        check_id = check.get("id")
        _require(isinstance(check_id, str) and check_id, "acceptance check id is missing")
        _require(check_id not in seen, "acceptance check id is duplicated")
        seen.add(check_id)
    canonical_checks: list[dict[str, Any]] = []
    for check_id in sorted(seen):
        check = next(item for item in checks if item["id"] == check_id)
        # Keep the Go verifier.Check JSON field order and omit zero values.
        ordered: dict[str, Any] = {"id": check["id"], "type": check.get("type", "")}
        for field in ("path", "sha256", "recipe"):
            if check.get(field):
                ordered[field] = check[field]
        if check.get("require_untruncated") is True:
            ordered["require_untruncated"] = True
        _require(set(check).issubset({"id", "type", "path", "sha256", "recipe", "require_untruncated"}),
                 "acceptance check has unknown fields")
        lines.append(check_id + "=" + json.dumps(ordered, separators=(",", ":"), ensure_ascii=False))
        canonical_checks.append(ordered)
    canonical_spec = json.dumps({"version": spec["version"], "checks": canonical_checks},
                                separators=(",", ":"), ensure_ascii=False)
    _require(spec_raw == canonical_spec, "acceptance plan bytes are not canonical")
    return hashlib.sha256("\n".join(lines).encode("utf-8")).hexdigest()


def _scan_secret(state_dir: Path, secret: str) -> None:
    secret_bytes = secret.encode("utf-8")
    _require(bool(secret_bytes), "secret scan cannot use an empty key")
    files: list[Path] = []
    try:
        for root, dirs, names in os.walk(state_dir, followlinks=False):
            root_path = Path(root)
            for name in list(dirs):
                child = root_path / name
                _require(not child.is_symlink(), "secret scan encountered a symlink")
            for name in names:
                child = root_path / name
                _require(not child.is_symlink(), "secret scan encountered a symlink")
                _require(child.is_file(), "secret scan encountered a non-regular retained-state entry")
                files.append(child)
                _require(len(files) <= MAX_STATE_FILES, "secret scan file-count bound exceeded")
        total = 0
        for path in sorted(files):
            size = path.stat().st_size
            _require(size <= MAX_STATE_FILE_BYTES, "secret scan per-file bound exceeded")
            total += size
            _require(total <= MAX_STATE_TOTAL_BYTES, "secret scan total-size bound exceeded")
            tail = b""
            read_size = 0
            with path.open("rb") as stream:
                while True:
                    chunk = stream.read(SCAN_CHUNK_BYTES)
                    if not chunk:
                        break
                    read_size += len(chunk)
                    _require(read_size <= MAX_STATE_FILE_BYTES and
                             total - size + read_size <= MAX_STATE_TOTAL_BYTES,
                             "secret scan read bound exceeded")
                    data = tail + chunk
                    if secret_bytes in data:
                        raise AuditError("secret scan found the configured key in retained state")
                    tail = data[-max(0, len(secret_bytes) - 1):] if len(secret_bytes) > 1 else b""
    except AuditError:
        raise
    except OSError as exc:
        raise AuditError("secret scan could not read bounded retained state") from exc


def _count(conn: sqlite3.Connection, table: str, task_id: str) -> int:
    try:
        row = conn.execute(f"SELECT COUNT(*) FROM {table} WHERE task_id = ?", (task_id,)).fetchone()
    except sqlite3.Error as exc:
        raise AuditError(f"required SQLite table {table} is unavailable") from exc
    _require(row is not None, f"required SQLite count for {table} is missing")
    return int(row[0])


def _audit_database(db_path: Path, task_id: str) -> dict[str, Any]:
    try:
        conn = sqlite3.connect(db_path.as_uri() + "?mode=ro", uri=True)
        conn.row_factory = sqlite3.Row
        conn.execute("PRAGMA query_only=ON")
        _require(conn.execute("PRAGMA query_only").fetchone()[0] == 1,
                 "SQLite query_only could not be enabled")
    except (sqlite3.Error, OSError) as exc:
        raise AuditError("cannot open retained SQLite database read-only") from exc

    try:
        task_rows = conn.execute(
            "SELECT task_id,objective,status,outcome,stop_reason,resume_count,config_json,"
            "execution_contract_json,execution_contract_hash FROM tasks WHERE task_id=?", (task_id,)
        ).fetchall()
        _require(len(task_rows) == 1, "task identity is missing or duplicated")
        task = task_rows[0]
        _require(task["task_id"] == TASK_ID == task_id, "task id differs")
        _require(task["objective"] == OBJECTIVE, "task objective differs")
        _require(task["status"] == "failed" and task["outcome"] == "provider_failure",
                 "terminal task status or outcome differs")
        _require(task["stop_reason"] == "provider failure: uncertain_reached", "terminal stop reason differs")
        _require(task["resume_count"] == 0, "resume_count differs")

        config = _decode_object(task["config_json"], "task config")
        expected_config = {
            "provider_id": PROVIDER_ID,
            "protocol_family": PROTOCOL_FAMILY,
            "provider_model": MODEL,
            "model": MODEL,
            "provider_profile_version": "v1",
            "provider_adapter_version": ADAPTER_VERSION,
        }
        for field, expected in expected_config.items():
            _require(config.get(field) == expected, f"task config {field} differs")
        config_identity = config.get("provider_config_identity")
        _require(isinstance(config_identity, str) and bool(config_identity),
                 "task config identity is missing")

        contract_raw = task["execution_contract_json"]
        contract_hash = task["execution_contract_hash"]
        _require(bool(contract_raw) and isinstance(contract_hash, str) and contract_hash.startswith("sha256:"),
                 "execution contract identity is missing")
        actual_contract_hash = "sha256:" + hashlib.sha256(contract_raw.encode("utf-8")).hexdigest()
        _require(actual_contract_hash == contract_hash, "execution contract hash differs from its persisted bytes")
        contract = _decode_object(contract_raw, "execution contract")
        contract_provider = contract.get("provider")
        _require(isinstance(contract_provider, dict), "execution contract provider identity is missing")
        for field, expected in {
            "provider_id": PROVIDER_ID,
            "protocol_family": PROTOCOL_FAMILY,
            "model": MODEL,
            "provider_profile_version": "v1",
            "adapter_version": ADAPTER_VERSION,
            "config_identity": config_identity,
        }.items():
            _require(contract_provider.get(field) == expected, f"execution contract {field} differs")

        attempt_rows = conn.execute(
            "SELECT execution_id,client_request_id,provider,model_pool,model,attempt_sequence,status,outcome,"
            "upstream_reached,uncertain,attempt_debited,selected_backoff_ns,delivery_state,protocol_family,config_identity,"
            "request_id,error_class,prepared_at,completed_at FROM provider_attempts WHERE task_id=? "
            "ORDER BY attempt_sequence", (task_id,)
        ).fetchall()
        _require(len(attempt_rows) == 1, "physical provider attempt count differs")
        attempt = attempt_rows[0]
        for field, expected in {
            "execution_id": "exec-000001",
            "client_request_id": task_id + "-0001",
            "provider": PROVIDER_ID,
            "model_pool": "instant",
            "model": MODEL,
            "attempt_sequence": 1,
            "status": "uncertain",
            "outcome": "uncertain_reached",
            "upstream_reached": 1,
            "uncertain": 1,
            "attempt_debited": 1,
            "selected_backoff_ns": 0,
            "delivery_state": "sent_unconfirmed",
            "protocol_family": PROTOCOL_FAMILY,
            "config_identity": config_identity,
        }.items():
            _require(attempt[field] == expected, f"provider attempt {field} differs")

        ledger_rows = conn.execute("SELECT id FROM governor_ledger WHERE task_id=? ORDER BY id", (task_id,)).fetchall()
        _require(len(ledger_rows) == 1, "governor ledger admission count differs")
        governor_rows = conn.execute(
            "SELECT attempts,retries FROM governor_task_states WHERE task_id=?", (task_id,)
        ).fetchall()
        _require(len(governor_rows) == 1 and governor_rows[0]["attempts"] == 1 and governor_rows[0]["retries"] == 0,
                 "governor attempts or retries differ")

        event_rows = conn.execute(
            "SELECT sequence,kind,payload_json,created_at FROM events WHERE task_id=? ORDER BY sequence", (task_id,)
        ).fetchall()
        events: list[tuple[int, str, dict[str, Any], str]] = []
        for row in event_rows:
            events.append((row["sequence"], row["kind"], _decode_object(row["payload_json"], "task event"), row["created_at"]))
        prepared = [event for event in events if event[1] == "provider_attempt_prepared"]
        terminal = [event for event in events if event[1] == "provider_attempt_uncertain"]
        provider_events = [event for event in events if event[1].startswith("provider_attempt_")]
        _require(len(prepared) == 1 and len(terminal) == 1 and len(provider_events) == 2,
                 "provider event chronology contains a missing or additional attempt")
        pre, end = prepared[0], terminal[0]
        _require(pre[0] < end[0], "provider attempt event ordering differs")
        _require(pre[2].get("execution_id") == attempt["execution_id"] and
                 pre[2].get("client_request_id") == attempt["client_request_id"],
                 "prepared event does not identify the retained attempt")
        for event, expected in (
            (pre, {"provider": PROVIDER_ID, "model": MODEL, "protocol_family": PROTOCOL_FAMILY,
                   "config_identity": config_identity, "attempt_sequence": 1, "model_pool": "instant"}),
            (end, {"status": "uncertain", "outcome": "uncertain_reached", "upstream_reached": True,
                   "uncertain": True, "delivery_state": "sent_unconfirmed", "attempt_debited": 1,
                   "selected_backoff": 0, "protocol_family": PROTOCOL_FAMILY,
                   "config_identity": config_identity}),
        ):
            for field, value in expected.items():
                _require(event[2].get(field) == value, f"provider event {field} differs")
            governor = event[2].get("governor")
            _require(isinstance(governor, dict) and governor.get("next_attempt") == 2 and
                     governor.get("task_used") == 1 and governor.get("rolling_10m") == 1 and
                     governor.get("rolling_1h") == 1 and governor.get("rolling_3h") == 1,
                     "governor admission metadata differs")
        _require(end[2].get("client_request_id") == attempt["client_request_id"],
                 "uncertain event does not identify the retained attempt")
        _require(end[2].get("request_id", "") == attempt["request_id"],
                 "upstream request metadata differs between event and attempt")
        receipt_rows = conn.execute(
            "SELECT COUNT(*) FROM provider_attempt_receipts WHERE task_id=?", (task_id,)
        ).fetchone()
        _require(receipt_rows is not None and receipt_rows[0] == end[2].get("receipts") == 0,
                 "upstream receipt metadata differs from persisted receipts")

        acceptance_rows = conn.execute(
            "SELECT version,spec_json,digest,created_at FROM acceptance_plans WHERE task_id=? ORDER BY version",
            (task_id,),
        ).fetchall()
        _require(len(acceptance_rows) == 1, "persisted acceptance plan count differs")
        acceptance = acceptance_rows[0]
        actual_acceptance_digest = _acceptance_digest(acceptance["spec_json"])
        _require(actual_acceptance_digest == acceptance["digest"] == EXPECTED_ACCEPTANCE_DIGEST,
                 "acceptance plan digest differs")
        acceptance_events = [event for event in events if event[1] == "acceptance_plan_saved"]
        _require(len(acceptance_events) == 1, "acceptance save history is missing or was repeated")
        saved = acceptance_events[0]
        _require(saved[0] < pre[0] and saved[3] <= pre[3] and acceptance["created_at"] <= attempt["prepared_at"],
                 "acceptance plan was not persisted before provider dispatch")
        _require(saved[2].get("digest") == acceptance["digest"] and
                 config.get("acceptance_plan_digest") == acceptance["digest"] and
                 contract.get("acceptance_plan_digest") == acceptance["digest"],
                 "acceptance digest differs across persisted task identities")
        _require(not any(event[0] > pre[0] and event[1] == "acceptance_plan_saved" for event in events),
                 "acceptance plan changed after dispatch")
        finalized = [event for event in events if event[1] == "task_finalized"]
        _require(len(finalized) == 1 and finalized[0][2].get("status") == task["status"] and
                 finalized[0][2].get("outcome") == task["outcome"],
                 "terminal task event differs from task projection")

        counts = {table: _count(conn, table, task_id) for table in
                  ("actions", "tool_attempts", "tool_results", "verification_attempts")}
        _require(all(value == 0 for value in counts.values()), "unexpected action, tool, result, or verifier row")
        for table in ("actions", "tool_attempts"):
            try:
                tool_names = conn.execute(f"SELECT tool FROM {table} WHERE task_id=?", (task_id,)).fetchall()
            except sqlite3.Error as exc:
                raise AuditError(f"required SQLite table {table} is unavailable") from exc
            _require(not any(any(term in (row[0] or "").lower() for term in ("recipe", "process"))
                             for row in tool_names), f"unexpected recipe or process in {table}")
        _require(attempt["request_id"] == "", "unexpected upstream request metadata")
        _require(attempt["error_class"] == "", "unexpected persisted transport classification")
        _require(end[2].get("receipt_error") == "",
                 "unexpected upstream receipt metadata")

        return {
            "audit_result": "PASS",
            "task_id": task_id,
            "status": task["status"],
            "outcome": task["outcome"],
            "stop_reason": "uncertain_reached",
            "resume_count": task["resume_count"],
            "execution_contract_hash": contract_hash,
            "provider_id": PROVIDER_ID,
            "protocol_family": PROTOCOL_FAMILY,
            "model": MODEL,
            "config_identity_sha256": hashlib.sha256(config_identity.encode("utf-8")).hexdigest(),
            "provider_attempts": 1,
            "attempt_sequence": 1,
            "attempt_status": attempt["status"],
            "attempt_outcome": attempt["outcome"],
            "upstream_reached": attempt["upstream_reached"],
            "uncertain": attempt["uncertain"],
            "attempt_debited": attempt["attempt_debited"],
            "model_pool": attempt["model_pool"],
            "selected_backoff_ns": attempt["selected_backoff_ns"],
            "delivery_state": attempt["delivery_state"],
            "request_id_present": bool(attempt["request_id"]),
            "error_class": "empty",
            "receipt_count": receipt_rows[0],
            "receipt_error": "empty",
            "governor_ledger_entries": len(ledger_rows),
            "governor_admissions": governor_rows[0]["attempts"],
            "retries": governor_rows[0]["retries"],
            "prepared_event_sequence": pre[0],
            "uncertain_event_sequence": end[0],
            "event_ordering": "prepared_then_uncertain",
            "actions": counts["actions"],
            "tool_attempts": counts["tool_attempts"],
            "tool_results": counts["tool_results"],
            "writes": 0,
            "recipes_processes": 0,
            "verification_attempts": counts["verification_attempts"],
            "acceptance_digest": acceptance["digest"],
            "acceptance_saved_before_dispatch": True,
            "acceptance_changes_after_dispatch": 0,
            "transport_root_cause": "unknown",
            "original_cli_output": "not_retained",
            "additional_provider_requests": 0,
        }
    except sqlite3.Error as exc:
        raise AuditError("SQLite audit query failed") from exc
    finally:
        conn.close()


def audit_state(state_dir: Path | str, task_id: str, secret: str | None) -> dict[str, Any]:
    root = Path(state_dir)
    _require(root.is_dir() and not root.is_symlink(), "state directory is missing or unsafe")
    db_path = root / "runstead.db"
    _require(db_path.is_file() and not db_path.is_symlink(), "retained SQLite database is missing or unsafe")
    result = _audit_database(db_path, task_id)
    if secret:
        _scan_secret(root, secret)
        result["secret_scan"] = "clean"
    else:
        result["secret_scan"] = "unavailable"
        result["audit_result"] = "LIMITED"
    return result


def _render(result: dict[str, Any]) -> str:
    order = (
        "audit_result", "task_id", "status", "outcome", "stop_reason", "resume_count",
        "execution_contract_hash", "provider_id", "protocol_family", "model", "config_identity_sha256",
        "provider_attempts", "attempt_sequence", "attempt_status", "attempt_outcome", "upstream_reached", "uncertain",
        "attempt_debited", "model_pool", "selected_backoff_ns", "delivery_state", "request_id_present",
        "error_class", "receipt_count", "receipt_error",
        "governor_ledger_entries", "governor_admissions", "retries", "prepared_event_sequence",
        "uncertain_event_sequence", "event_ordering", "actions", "tool_attempts", "tool_results", "writes",
        "recipes_processes", "verification_attempts", "acceptance_digest", "acceptance_saved_before_dispatch",
        "acceptance_changes_after_dispatch", "secret_scan", "transport_root_cause", "original_cli_output",
        "additional_provider_requests",
    )
    return "\n".join(f"{key}={result[key]}" for key in order if key in result)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--state-dir", required=True, type=Path)
    parser.add_argument("--task-id", required=True)
    args = parser.parse_args(argv)
    if args.task_id != TASK_ID:
        print("audit_result=FAIL\nreason=task_id_differs", file=sys.stderr)
        return 1
    try:
        result = audit_state(args.state_dir, args.task_id, os.environ.get("GROQ_API_KEY") or None)
    except AuditError as exc:
        print("audit_result=FAIL", file=sys.stderr)
        print(f"reason={exc}", file=sys.stderr)
        return 1
    print(_render(result))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
