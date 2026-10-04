#!/usr/bin/env python3
"""Run one Stage 2 task or inspect an existing experiment task's evidence.

The model's final note and Runstead stdout/stderr are held only in process
memory. This runner prints bounded identifiers and predicates, never the note,
provider response, credential, or raw SQLite rows.

This is an experiment evidence checker only. It is not Runstead's verifier,
policy, governor, or durable truth.
"""
import argparse
import hashlib
import json
import os
import re
import shutil
import sqlite3
import stat
import subprocess
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[2]
OBJECTIVE = (
    "Read app/calc.go and identify the whitespace-handling bug in ParseValues. "
    "Do not modify files and do not run recipes/processes. "
    "Base the final claim on the actual read_file evidence."
)
PROVIDER_ID = "groq-gpt-oss-120b-canary-v2"
MODEL = "openai/gpt-oss-120b"
FAMILY = "openai_compatible"
READ_TOOLS = {"read_file", "list_files", "search_text", "git_status", "git_diff"}
FORBIDDEN_TOOLS = {"write_file", "apply_patch", "run_recipe"}
PROCESS_BACKED_TOOLS = {"search_text", "git_status", "git_diff"}
NON_PROCESS_TOOLS = {"read_file", "list_files"}
EXPECTED_ADAPTER_VERSION = "compatible-provider-v0.1"
EXPECTED_TASK_ID = "cli-1791085826705830653"
EXPECTED_EXECUTION_ID = "exec-000001"
EXPECTED_CLIENT_REQUEST_ID = EXPECTED_TASK_ID + "-0001"
EXPECTED_CONFIG_IDENTITY = (
    'provider.Config{ProviderID:"groq-gpt-oss-120b-canary-v2" '
    'ProtocolFamily:"openai_compatible" Endpoint:"https://api.groq.com/openai/v1" '
    'Model:"openai/gpt-oss-120b" AuthRequirement:"reference_required" AuthRef:true '
    'Options:[] ProfileVersion:"v1" RouteSafety:provider.RouteSafety{AttemptAccounting:0x1, '
    'SingleAttempt:0x1, InternalRetries:0x1, CooldownReplay:0x1, AccountPooling:0x1, '
    'AutomaticFallback:0x1, ComboRouting:0x1} ConfigVersion:"v1"}'
)
MAX_SCAN_FILES = 256
MAX_SCAN_ENTRIES = 2048
MAX_SCAN_FILE_BYTES = 8 * 1024 * 1024
MAX_SCAN_TOTAL_BYTES = 64 * 1024 * 1024
CLAIM_TEMPLATE = re.compile(
    r"^\s*[^.!?;\n]{0,80}\b(?:bug|defect|issue)\b[^.!?;\n]*\bparsevalues\b"
    r"[^.!?;\n]*\bstrconv\.atoi\(part\)[^.!?;\n]*"
    r"\bwithout (?:trimming|trim|strings\.trimspace)\b[^.!?;\n]*"
    r"\b(?:whitespace|spaces?)\b[^.!?;\n]*\bfails? to parse[.!]?\s*$"
)
EXPECTED_PACKAGE = {
    "id": "repo.read", "version": "1.0.0", "provenance": "runstead/builtin",
    "kind": "builtin", "runtime_compatibility": "runstead-runtime.v1",
    "actions": ["git_diff", "git_status", "list_files", "read_file", "search_text"],
    "capabilities": ["git_metadata", "read_workspace"],
    "workspace_requirements": ["workspace"], "network_requirements": None,
    "effect_class": "read_only", "recovery_class": "replay_safe",
    "evidence_requirements": ["observation"],
    "verification_requirements": ["runstead.verifier.v1"],
    "approval_boundary": "none", "max_output_bytes": 8192,
}
EXPECTED_TOOLS = [
    {"name": "git_diff", "summary": "Show unstaged working tree changes.", "read_only": True},
    {"name": "git_status", "summary": "Show the repository working tree status.", "read_only": True},
    {"name": "list_files", "summary": "List one directory level inside the workspace.", "read_only": True,
     "arguments": [{"name": "path", "type": "string", "required": True,
                    "note": "relative directory path inside the workspace"}]},
    {"name": "read_file", "summary": "Read a UTF-8 text file inside the workspace.", "read_only": True,
     "arguments": [{"name": "path", "type": "string", "required": True,
                    "note": "relative path inside the workspace"}]},
    {"name": "search_text", "summary": "Search for fixed text inside the workspace.", "read_only": True,
     "arguments": [{"name": "path", "type": "string", "required": True,
                    "note": "relative path to search under"},
                   {"name": "query", "type": "string", "required": True,
                    "note": "fixed text to search for"}]},
]


def stop(reason: str) -> None:
    raise RuntimeError(reason)


def sha(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def load_json(path: Path):
    return json.loads(path.read_text(encoding="utf-8"))


def reject_duplicate_keys(pairs):
    value = {}
    for key, item in pairs:
        if key in value:
            raise ValueError("duplicate JSON key")
        value[key] = item
    return value


def canonical_contract_bytes(contract: dict) -> bytes:
    top_order = [
        "contract_version", "runtime_identity", "protocol_identity", "profile", "packages",
        "provider", "tools", "tool_schema_digest", "recipe_catalog", "write_policy_identity",
        "recipe_policy_identity", "acceptance_plan_digest", "governor_identity", "policy_identity",
        "evidence_identity", "recovery_identity", "verifier_identity",
    ]
    required = set(top_order) - {"write_policy_identity", "recipe_policy_identity", "acceptance_plan_digest"}
    if not required <= set(contract) or set(contract) - set(top_order):
        stop("persisted execution contract has missing or unknown fields")
    if contract.get("packages") != [EXPECTED_PACKAGE] or contract.get("tools") != EXPECTED_TOOLS:
        stop("persisted package identity or exact effective tool schema differs from repo.read@1.0.0")

    def ordered(value, fields, optional=()):
        if set(value) - set(fields) or (set(fields) - set(optional)) - set(value):
            stop("persisted execution contract has a noncanonical nested shape")
        return {field: value[field] for field in fields if field in value}

    profile = ordered(contract["profile"], ["id", "version"])
    provider = ordered(contract["provider"], [
        "provider_id", "protocol_family", "model", "config_identity",
        "provider_profile_version", "adapter_version",
    ])
    package_fields = [
        "id", "version", "provenance", "kind", "runtime_compatibility", "actions",
        "capabilities", "workspace_requirements", "network_requirements", "effect_class",
        "recovery_class", "evidence_requirements", "verification_requirements",
        "approval_boundary", "max_output_bytes", "dependencies", "conflicts",
    ]
    package = ordered(contract["packages"][0], package_fields, optional=("dependencies", "conflicts"))
    tool_list = []
    for tool in contract["tools"]:
        canonical_tool = ordered(tool, ["name", "summary", "read_only", "arguments"], optional=("arguments",))
        if "arguments" in canonical_tool:
            canonical_tool["arguments"] = [
                ordered(argument, ["name", "type", "required", "note"])
                for argument in canonical_tool["arguments"]
            ]
        tool_list.append(canonical_tool)
    recipe_catalog = ordered(contract["recipe_catalog"], ["digest", "recipe_ids"], optional=("digest", "recipe_ids"))
    if ("recipe_ids" in recipe_catalog and not recipe_catalog["recipe_ids"]) or any(
            not contract[field] for field in ("write_policy_identity", "recipe_policy_identity", "acceptance_plan_digest")
            if field in contract):
        stop("persisted execution contract includes a noncanonical empty optional field")
    result = {}
    for field in top_order:
        if field not in contract:
            continue
        value = contract[field]
        if field == "profile":
            value = profile
        elif field == "packages":
            value = [package]
        elif field == "provider":
            value = provider
        elif field == "tools":
            value = tool_list
        elif field == "recipe_catalog":
            value = recipe_catalog
        result[field] = value
    try:
        return json.dumps(result, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
    except (TypeError, ValueError):
        stop("persisted execution contract cannot be canonicalized")


def contains_bytes(directory: Path, value: bytes) -> bool:
    if not value:
        return False
    for candidate in directory.rglob("*"):
        if candidate.is_file():
            try:
                if value in candidate.read_bytes():
                    return True
            except OSError:
                stop("cannot complete the bounded state secrecy scan")
    return False


def static_preflight() -> str:
    provider_doc = load_json(HERE / "providers.json")
    if provider_doc.get("version") != 1 or len(provider_doc.get("providers", [])) != 1:
        stop("candidate declaration is not one v1 provider")
    candidate = provider_doc["providers"][0]
    expected = {
        "provider_id": PROVIDER_ID,
        "protocol_family": FAMILY,
        "base_url": "https://api.groq.com/openai/v1",
        "model": MODEL,
        "auth_requirement": "reference_required",
        "auth_ref": "GROQ_API_KEY",
    }
    if any(candidate.get(k) != v for k, v in expected.items()):
        stop("candidate differs from issue #141")
    safety = candidate.get("profile", {}).get("route_safety", {})
    if safety.get("attempt_accounting") != "single" or safety.get("single_attempt") != "guaranteed":
        stop("route is not declared single-attempt")
    if any(safety.get(k) != "disabled" for k in (
        "internal_retries", "cooldown_replay", "account_pooling", "automatic_fallback", "combo_routing"
    )):
        stop("route declares an amplification path")

    contract = load_json(HERE / "stage2-preflight.json")
    profile = load_json(HERE / contract["profile"])
    if contract.get("objective") != OBJECTIVE:
        stop("task objective differs from the reviewed objective")
    if contract.get("workspace") != "fresh temporary copy of fixtures/coding-loop; do not initialize Git so verifier Git observation launches no process":
        stop("workspace setup does not preserve the no-process boundary")
    if profile.get("provider_id") != PROVIDER_ID or profile.get("packages") != [
        {"id": "repo.read", "version": "1.0.0"}
    ] or profile.get("recipe_ids"):
        stop("profile does not expose only the read-only package")
    if set(contract.get("allowed_tools", [])) != READ_TOOLS:
        stop("tool list differs from repo.read@1.0.0")
    if contract.get("independent_audit_runner") != Path(__file__).name:
        stop("preflight does not name this independent auditor")
    post = contract.get("post_run_verification", {})
    if post.get("required_action") != {"tool": "read_file", "path": "app/calc.go", "success": True}:
        stop("independent verifier does not require the target read_file action")
    if post.get("required_observation") != {
        "data_path": "app/calc.go", "metadata_path": "app/calc.go", "sha256_matches_fixture": True
    }:
        stop("independent verifier does not require target observation and hash")
    if post.get("final_claim") != {
        "requires": [
            "bug or defect assertion", "ParseValues", "strconv.Atoi(part)", "positive no-trim statement",
            "positive whitespace parse-failure statement",
        ],
        "single_sentence_full_match": True,
        "rejects_contradiction_markers": ["not", "never", "but", "however", "actually", "except", "unless", "although"],
        "citation_must_match_observation": True,
    }:
        stop("independent verifier does not bind the claim to its citation")
    if post.get("prohibited_effect_tools") != sorted(FORBIDDEN_TOOLS):
        stop("independent verifier does not reject prohibited effects")
    if post.get("process_backed_tools") != sorted(PROCESS_BACKED_TOOLS):
        stop("independent verifier does not reject process-backed observation tools")
    if contract.get("process_boundary") != {
        "task_actions": "no process-backed tools or recipes; only read_file/list_files may be attempted",
        "control_plane": "Git and ripgrep are intentionally unavailable on the isolated task PATH, so bootstrap/verifier and observation tools cannot launch subprocesses",
        "qualification": "any search_text, git_status, git_diff, run_recipe, write_file, or apply_patch action/attempt fails Stage 2",
    }:
        stop("process boundary is not explicit and enforceable")
    if post.get("terminal") != {
        "task_status": "completed", "latest_verifier": "passed", "acceptance_check": "calc-source-unchanged"
    }:
        stop("independent verifier does not require verified completion")
    if post.get("attempt_accounting") != {
        "physical_requests_equal_admissions": True, "debits_equal_admissions": True, "retries": 0
    }:
        stop("independent verifier does not reconcile requests and debits")

    fixture = ROOT / "fixtures/coding-loop"
    source = (fixture / "app/calc.go").read_bytes()
    source_hash = sha(source)
    plan = load_json(HERE / contract["acceptance_plan"])
    expected_check = {
        "id": "calc-source-unchanged", "type": "file_hash",
        "path": "app/calc.go", "sha256": source_hash,
    }
    if plan.get("version") != 1 or plan.get("checks") != [expected_check]:
        stop("acceptance does not bind the objective file to the committed fixture")
    code = source.decode("utf-8")
    parse_values = code.split("func ParseValues(", 1)[1].split("\n}", 1)[0]
    tests = (fixture / "app/calc_test.go").read_text(encoding="utf-8")
    if "strings.TrimSpace(" in parse_values or "strconv.Atoi(part)" not in parse_values:
        stop("fixture does not contain the stated ParseValues whitespace defect")
    if 'ParseValues("1, 2 , 3")' not in tests:
        stop("fixture tests do not assert whitespace handling")
    return source_hash


def final_note(stdout: str) -> tuple[str, list[str]]:
    lines = stdout.splitlines()
    start = next((i for i, line in enumerate(lines) if line.startswith("note (unverified): ")), None)
    if start is None:
        stop("completed CLI output has no final note")
    note_parts = [lines[start].split(": ", 1)[1]]
    evidence = []
    for line in lines[start + 1:]:
        if line.startswith("evidence: "):
            evidence.append(line.split(": ", 1)[1].strip())
        elif evidence or line.startswith("Verified runtime result:"):
            break
        else:
            note_parts.append(line)
    note = "\n".join(note_parts).strip()
    if not evidence:
        stop("completed CLI output has no final evidence citation")
    return note, evidence


def audit_database(db_path: Path, task_id: str, note: str, cited_ids: list[str], expected_hash: str) -> dict:
    uri = f"file:{db_path.resolve()}?mode=ro"
    db = sqlite3.connect(uri, uri=True)
    db.row_factory = sqlite3.Row
    try:
        task = db.execute(
            "SELECT objective,status,outcome,resume_count,model,config_json,workspace,execution_contract_json,execution_contract_hash FROM tasks WHERE task_id=?",
            (task_id,),
        ).fetchone()
        if task is None:
            stop("task ID has no durable task row")
        if (task["objective"] != OBJECTIVE or task["status"] != "completed" or
                task["outcome"] != "completed" or task["resume_count"] != 0 or task["model"] != MODEL):
            stop("task objective, identity, or terminal state does not match Stage 2")
        config_json = task["config_json"]
        secret = os.environ.get("GROQ_API_KEY", "")
        if secret and secret in config_json:
            stop("secret value found in persisted task config")

        contract_bytes = task["execution_contract_json"].encode("utf-8")
        contract_hash = "sha256:" + sha(contract_bytes)
        if task["execution_contract_hash"] != contract_hash:
            stop("persisted execution contract hash does not match its exact bytes")
        execution_contract = json.loads(contract_bytes, object_pairs_hook=reject_duplicate_keys)
        canonical_bytes = canonical_contract_bytes(execution_contract)
        if canonical_bytes != contract_bytes:
            stop("persisted execution contract bytes are not canonical")
        contract_tools = execution_contract.get("tools", [])
        contract_tool_names = {tool.get("name") for tool in contract_tools}
        expected_tool_names = sorted(READ_TOOLS)
        packages = execution_contract.get("packages", [])
        if (execution_contract.get("contract_version") != 1 or
                execution_contract.get("runtime_identity") != "runstead-runtime.v1" or
                execution_contract.get("protocol_identity") != "runstead.protocol.v1" or
                execution_contract.get("profile") != {"id": "groq-canary-v2-read-only", "version": "1.0.0"} or
                packages != [EXPECTED_PACKAGE] or
                contract_tool_names != set(expected_tool_names) or
                contract_tools != EXPECTED_TOOLS or
                execution_contract.get("tool_schema_digest") != "sha256:" + sha(
                    json.dumps(EXPECTED_TOOLS, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
                ) or
                any(tool.get("read_only") is not True for tool in contract_tools) or
                execution_contract.get("recipe_catalog") != {} or
                execution_contract.get("provider", {}).get("provider_id") != PROVIDER_ID or
                execution_contract.get("provider", {}).get("protocol_family") != FAMILY or
                execution_contract.get("provider", {}).get("model") != MODEL or
                not execution_contract.get("provider", {}).get("config_identity") or
                execution_contract.get("provider", {}).get("provider_profile_version") != "v1" or
                not execution_contract.get("provider", {}).get("adapter_version") or
                execution_contract.get("governor_identity") != "runstead.governor.v1" or
                execution_contract.get("policy_identity") != "runstead.policy.v1" or
                execution_contract.get("evidence_identity") != "runstead.evidence.v1" or
                execution_contract.get("recovery_identity") != "runstead.recovery.v1" or
                execution_contract.get("verifier_identity") != "runstead.verifier.v1"):
            stop("persisted execution contract differs from the reviewed read-only composition")

        plan_row = db.execute("SELECT spec_json,digest FROM acceptance_plans WHERE task_id=?", (task_id,)).fetchone()
        if plan_row is None:
            stop("persisted acceptance plan is missing")
        stored_plan = json.loads(plan_row["spec_json"])
        if stored_plan != {"version": 1, "checks": [{
            "id": "calc-source-unchanged", "type": "file_hash", "path": "app/calc.go", "sha256": expected_hash
        }]}:
            stop("persisted acceptance plan differs from the committed preflight")
        check_json = json.dumps(stored_plan["checks"][0], ensure_ascii=False, separators=(",", ":"))
        plan_digest = sha(f"version=1\ncalc-source-unchanged={check_json}".encode("utf-8"))
        if (plan_row["digest"] != plan_digest or
                execution_contract.get("acceptance_plan_digest") != plan_digest):
            stop("acceptance plan digest is not pinned by the frozen execution contract")

        actions = db.execute(
            "SELECT action_id,tool,arguments_json,status FROM actions WHERE task_id=? ORDER BY action_sequence",
            (task_id,),
        ).fetchall()
        if not actions or any(row["tool"] not in NON_PROCESS_TOOLS for row in actions):
            stop("actions include a process-backed, prohibited, or unknown tool")
        attempts = db.execute(
            "SELECT execution_id,action_id,tool,arguments_json,status,evidence_id FROM tool_attempts WHERE task_id=? ORDER BY created_at,execution_id",
            (task_id,),
        ).fetchall()
        if not attempts or any(row["tool"] in FORBIDDEN_TOOLS or row["tool"] not in NON_PROCESS_TOOLS or
                               row["status"] in {"prepared", "running", "uncertain", "human_review_required"}
                               for row in attempts):
            stop("tool attempts include a process-backed/write effect or unresolved effect")
        actions_by_id = {row["action_id"]: row["tool"] for row in actions}
        attempts_by_id = {}
        for row in attempts:
            attempts_by_id.setdefault(row["action_id"], []).append(row)
        if set(actions_by_id) != set(attempts_by_id) or any(
                len(rows) != 1 or actions_by_id.get(action_id) != rows[0]["tool"]
                for action_id, rows in attempts_by_id.items()):
            stop("actions and tool attempts do not reconcile one-to-one")

        matches = []
        for row in attempts:
            args = json.loads(row["arguments_json"])
            if row["tool"] == "read_file" and args.get("path") == "app/calc.go" and row["evidence_id"]:
                evidence = db.execute(
                    "SELECT execution_id,success,data_json,metadata_json FROM tool_results WHERE task_id=? AND evidence_id=?",
                    (task_id, row["evidence_id"]),
                ).fetchone()
                if evidence is None or evidence["execution_id"] != row["execution_id"] or evidence["success"] != 1:
                    continue
                data = json.loads(evidence["data_json"])
                metadata = json.loads(evidence["metadata_json"])
                if (data.get("path") != "app/calc.go" or metadata.get("path") != "app/calc.go" or
                        data.get("sha256") != expected_hash or not isinstance(data.get("content"), str) or
                        sha(data["content"].encode("utf-8")) != expected_hash):
                    continue
                content = data.get("content", "")
                body = content.split("func ParseValues(", 1)[1].split("\n}", 1)[0]
                if "strings.TrimSpace(" in body or "strconv.Atoi(part)" not in body:
                    continue
                matches.append((row, evidence, content))
        if not matches:
            stop("no successful read_file observation of app/calc.go was proven")
        cited_matches = [item for item in matches if item[0]["evidence_id"] in cited_ids]
        if not cited_matches:
            stop("final response did not cite the app/calc.go observation")
        attempt, observation, content = cited_matches[0]
        compact_note = re.sub(r"\s+", " ", note.casefold())
        contradiction = re.search(
            r"\b(?:not|never|but|however|actually|except|unless|although)\b", compact_note
        )
        if contradiction or not CLAIM_TEMPLATE.fullmatch(compact_note):
            stop("final claim does not positively identify untrimmed ParseValues input failing Atoi")

        verification = db.execute(
            "SELECT attempt_id,decision,report_json FROM verification_attempts WHERE task_id=? ORDER BY sequence DESC LIMIT 1",
            (task_id,),
        ).fetchone()
        if verification is None or verification["decision"] != "passed":
            stop("latest independent Runstead verifier did not pass")
        report = json.loads(verification["report_json"])
        checks = report.get("checks", [])
        if not any(c.get("id") == "calc-source-unchanged" and c.get("status") == "passed" for c in checks):
            stop("Runstead verifier did not pass the app/calc.go hash acceptance")
        claims = report.get("cited_evidence", [])
        if not any(c.get("evidence_id") == attempt["evidence_id"] and c.get("claimed_tool") == "read_file" and
                   c.get("tool") == "read_file" and c.get("exists") and c.get("tool_matches") for c in claims):
            stop("Runstead verifier did not ground the final citation to read_file evidence")

        provider_rows = db.execute(
            "SELECT execution_id,client_request_id,provider,protocol_family,config_identity,model,status,upstream_reached,uncertain,attempt_debited "
            "FROM provider_attempts WHERE task_id=? ORDER BY attempt_sequence,execution_id", (task_id,),
        ).fetchall()
        if not provider_rows or any(
            r["provider"] != PROVIDER_ID or r["protocol_family"] != FAMILY or r["model"] != MODEL or
            r["status"] != "completed" or r["upstream_reached"] != 1 or r["uncertain"] != 0 or r["attempt_debited"] != 1
            for r in provider_rows
        ):
            stop("provider attempts are not exact, completed, certain, and debited once")
        identities = {r["config_identity"] for r in provider_rows}
        if len(identities) != 1 or not next(iter(identities)):
            stop("provider config identity is missing or changed across requests")
        if execution_contract.get("provider", {}).get("config_identity") != next(iter(identities)):
            stop("provider attempts differ from the frozen provider config identity")
        count = len(provider_rows)
        task_usage = db.execute("SELECT attempts,retries FROM governor_task_states WHERE task_id=?", (task_id,)).fetchone()
        ledger = db.execute("SELECT count(*) FROM governor_ledger WHERE task_id=?", (task_id,)).fetchone()[0]
        prepared = db.execute("SELECT count(*) FROM events WHERE task_id=? AND kind='provider_attempt_prepared'", (task_id,)).fetchone()[0]
        completed = db.execute("SELECT count(*) FROM events WHERE task_id=? AND kind='provider_attempt_completed'", (task_id,)).fetchone()[0]
        if task_usage is None or task_usage["attempts"] != count or task_usage["retries"] != 0 or ledger != count or prepared != count or completed != count:
            stop("physical requests, admissions, ledger debits, and governor records do not reconcile")
        event_rows = db.execute(
            "SELECT sequence,kind,payload_json FROM events WHERE task_id=? ORDER BY sequence", (task_id,)
        ).fetchall()
        event_payloads = [(row["sequence"], row["kind"], json.loads(row["payload_json"])) for row in event_rows]
        for provider_row in provider_rows:
            prepared_events = [e for e in event_payloads if e[1] == "provider_attempt_prepared" and
                               e[2].get("execution_id") == provider_row["execution_id"]]
            completed_events = [e for e in event_payloads if e[1] == "provider_attempt_completed" and
                                e[2].get("client_request_id") == provider_row["client_request_id"]]
            if (len(prepared_events) != 1 or len(completed_events) != 1 or
                    prepared_events[0][0] >= completed_events[0][0] or
                    prepared_events[0][2].get("provider") != PROVIDER_ID or
                    prepared_events[0][2].get("protocol_family") != FAMILY or
                    prepared_events[0][2].get("config_identity") != provider_row["config_identity"] or
                    not isinstance(prepared_events[0][2].get("governor"), dict) or
                    completed_events[0][2].get("upstream_reached") is not True or
                    completed_events[0][2].get("attempt_debited") is not True or
                    completed_events[0][2].get("uncertain") is not False):
                stop("provider admission/completion events do not prove ordered exact execution")
        return {
            "task_id": task_id,
            "action_ids": [r["action_id"] for r in actions],
            "read_action_id": attempt["action_id"],
            "execution_id": attempt["execution_id"],
            "observation_id": attempt["evidence_id"],
            "verifier_id": verification["attempt_id"],
            "provider_attempts": count,
            "admissions": prepared,
            "debits": sum(r["attempt_debited"] for r in provider_rows),
            "governor_prepared_before_completed": count,
            "retries": task_usage["retries"],
            "config_identity": next(iter(identities)),
            "resume_count": task["resume_count"],
            "claim_sha256": sha(note.encode("utf-8")),
            "workspace": task["workspace"],
        }
    finally:
        db.close()


class AuditViolation(Exception):
    """A sanitized evidence predicate failed; never retain source row data."""

    def __init__(self, predicate: str):
        super().__init__(predicate)
        self.predicate = predicate


def require_audit(condition: bool, predicate: str) -> None:
    if not condition:
        raise AuditViolation(predicate)


def scan_secret(state_dir: Path) -> str:
    """Boundedly scan regular state files without following symlinks."""
    secret = os.environ.get("GROQ_API_KEY", "")
    if not secret:
        return "unavailable"
    needle = secret.encode("utf-8", errors="surrogateescape")
    if not needle:
        return "unavailable"
    nofollow = getattr(os, "O_NOFOLLOW", 0)
    directory_flag = getattr(os, "O_DIRECTORY", 0)
    if not nofollow or not directory_flag:
        return "incomplete"
    pending = []
    files_seen = entries_seen = bytes_seen = 0
    try:
        pending.append(os.open(state_dir, os.O_RDONLY | directory_flag | nofollow))
        while pending:
            directory_fd = pending.pop()
            try:
                with os.scandir(directory_fd) as entries:
                    for entry in entries:
                        entries_seen += 1
                        if entries_seen > MAX_SCAN_ENTRIES:
                            return "incomplete"
                        info = entry.stat(follow_symlinks=False)
                        if stat.S_ISLNK(info.st_mode):
                            return "incomplete"
                        if stat.S_ISDIR(info.st_mode):
                            child_fd = os.open(
                                entry.name, os.O_RDONLY | directory_flag | nofollow,
                                dir_fd=directory_fd,
                            )
                            if not stat.S_ISDIR(os.fstat(child_fd).st_mode):
                                os.close(child_fd)
                                return "incomplete"
                            pending.append(child_fd)
                            continue
                        if not stat.S_ISREG(info.st_mode):
                            continue
                        files_seen += 1
                        if files_seen > MAX_SCAN_FILES:
                            return "incomplete"
                        remaining = MAX_SCAN_TOTAL_BYTES - bytes_seen
                        if info.st_size > MAX_SCAN_FILE_BYTES or info.st_size > remaining:
                            return "incomplete"
                        file_fd = os.open(
                            entry.name, os.O_RDONLY | nofollow | getattr(os, "O_NONBLOCK", 0),
                            dir_fd=directory_fd,
                        )
                        with os.fdopen(file_fd, "rb") as source:
                            if not stat.S_ISREG(os.fstat(source.fileno()).st_mode):
                                return "incomplete"
                            limit = min(MAX_SCAN_FILE_BYTES, remaining)
                            data = source.read(limit + 1)
                        if len(data) > MAX_SCAN_FILE_BYTES or len(data) > remaining:
                            return "incomplete"
                        bytes_seen += len(data)
                        if needle in data:
                            return "present"
            finally:
                os.close(directory_fd)
    except OSError:
        return "incomplete"
    finally:
        for directory_fd in pending:
            try:
                os.close(directory_fd)
            except OSError:
                pass
    return "clean"


def audit_existing_database(db_path: Path, task_id: str) -> dict:
    """Check the known Stage 2 failure shape using SQLite read-only evidence."""
    uri = db_path.resolve().as_uri() + "?mode=ro"
    db = sqlite3.connect(uri, uri=True, timeout=5)
    db.row_factory = sqlite3.Row
    try:
        db.execute("PRAGMA query_only=ON")
        require_audit(db.execute("PRAGMA query_only").fetchone()[0] == 1, "sqlite_query_only")

        require_audit(task_id == EXPECTED_TASK_ID and
                      re.fullmatch(r"cli-[0-9]+", task_id) is not None, "task_id_exact_safe")
        expected_hash = static_preflight()
        task = db.execute(
            "SELECT task_id,objective,status,outcome,resume_count,model,config_json,execution_contract_json,execution_contract_hash "
            "FROM tasks WHERE task_id=?", (task_id,),
        ).fetchone()
        require_audit(task is not None, "task_row_present")
        require_audit(task["task_id"] == EXPECTED_TASK_ID, "persisted_task_id_exact_safe")
        require_audit(task["objective"] == OBJECTIVE, "task_objective_exact")
        require_audit(task["status"] == "failed" and task["outcome"] == "final_not_grounded",
                      "task_failure_shape")
        require_audit(task["resume_count"] == 0, "task_resume_count_zero")
        require_audit(task["model"] == MODEL, "task_model_exact")

        # Exit status is emitted by the CLI, not persisted in the task row.
        task_config = json.loads(task["config_json"])
        contract_bytes = task["execution_contract_json"].encode("utf-8")
        require_audit(task["execution_contract_hash"] == "sha256:" + sha(contract_bytes),
                      "frozen_contract_hash")
        frozen = json.loads(contract_bytes, object_pairs_hook=reject_duplicate_keys)
        require_audit(canonical_contract_bytes(frozen) == contract_bytes,
                      "frozen_contract_canonical")
        require_audit(frozen.get("contract_version") == 1 and
                      frozen.get("protocol_identity") == "runstead.protocol.v1",
                      "frozen_contract_identity")
        require_audit(frozen.get("profile") == {"id": "groq-canary-v2-read-only", "version": "1.0.0"},
                      "frozen_profile_identity")
        frozen_provider = frozen.get("provider", {})
        frozen_identity = frozen_provider.get("config_identity")

        stage_contract = load_json(HERE / "stage2-preflight.json")
        stage_provider = load_json(HERE / "providers.json")["providers"][0]
        profile = load_json(HERE / stage_contract["profile"])
        plan_row = db.execute(
            "SELECT version,spec_json,digest FROM acceptance_plans WHERE task_id=?", (task_id,),
        ).fetchone()
        require_audit(plan_row is not None, "acceptance_plan_present")
        stored_plan = json.loads(plan_row["spec_json"])
        expected_check = {
            "id": "calc-source-unchanged", "type": "file_hash",
            "path": "app/calc.go", "sha256": expected_hash,
        }
        expected_plan = {"version": 1, "checks": [expected_check]}
        require_audit(plan_row["version"] == 1 and stored_plan == expected_plan,
                      "acceptance_plan_exact")
        check_json = json.dumps(expected_check, ensure_ascii=False, separators=(",", ":"))
        expected_plan_digest = sha(f"version=1\ncalc-source-unchanged={check_json}".encode("utf-8"))
        require_audit(plan_row["digest"] == expected_plan_digest, "acceptance_plan_digest_exact")
        require_audit(stage_contract.get("acceptance_plan") == "stage2-acceptance.json" and
                      stage_contract.get("acceptance_checks") == [expected_check],
                      "stage2_preflight_acceptance_exact")

        config_provider_id = task_config.get("provider_id")
        config_family = task_config.get("protocol_family")
        config_model = task_config.get("provider_model")
        config_model_alias = task_config.get("model")
        config_identity = task_config.get("provider_config_identity")
        config_profile = task_config.get("provider_profile_version")
        config_adapter = task_config.get("provider_adapter_version")
        config_acceptance = task_config.get("acceptance_plan_digest")
        require_audit(
            config_provider_id == PROVIDER_ID == stage_provider.get("provider_id") == profile.get("provider_id") == frozen_provider.get("provider_id") and
            config_family == FAMILY == stage_provider.get("protocol_family") == frozen_provider.get("protocol_family") and
            config_model == MODEL == stage_provider.get("model") == frozen_provider.get("model") and
            config_model_alias == MODEL and config_profile == frozen_provider.get("provider_profile_version") == "v1" and
            config_adapter == frozen_provider.get("adapter_version") == EXPECTED_ADAPTER_VERSION and
            config_acceptance == plan_row["digest"] == frozen.get("acceptance_plan_digest") and
            config_identity == EXPECTED_CONFIG_IDENTITY and
            config_identity == frozen_identity,
            "task_config_matches_preflight_and_frozen_contract",
        )

        attempts = db.execute(
            "SELECT execution_id,client_request_id,provider,protocol_family,config_identity,model,attempt_sequence,status,outcome,delivery_state,request_id,upstream_reached,uncertain,attempt_debited "
            "FROM provider_attempts WHERE task_id=? ORDER BY attempt_sequence,execution_id", (task_id,),
        ).fetchall()
        require_audit(len(attempts) == 1, "provider_attempt_count_one")
        attempt = attempts[0]
        require_audit(
            attempt["provider"] == PROVIDER_ID and attempt["protocol_family"] == FAMILY and
            attempt["model"] == MODEL and attempt["status"] == "completed" and
            attempt["outcome"] == "success" and attempt["delivery_state"] == "completed" and
            re.fullmatch(r"sha256:[0-9a-f]{16}", attempt["request_id"] or "") is not None and
            attempt["upstream_reached"] == 1 and attempt["uncertain"] == 0 and
            attempt["attempt_debited"] == 1 and attempt["attempt_sequence"] == 1 and
            attempt["config_identity"] == config_identity == frozen_identity,
            "provider_attempt_exact_and_certain",
        )
        require_audit(attempt["execution_id"] == EXPECTED_EXECUTION_ID and
                      re.fullmatch(r"exec-[0-9]{6}", attempt["execution_id"] or "") is not None and
                      attempt["client_request_id"] == EXPECTED_CLIENT_REQUEST_ID and
                      attempt["client_request_id"] == f"{EXPECTED_TASK_ID}-{attempt['attempt_sequence']:04d}",
                      "provider_identifier_format")

        events = db.execute(
            "SELECT sequence,kind,payload_json FROM events WHERE task_id=? ORDER BY sequence", (task_id,),
        ).fetchall()
        acceptance_events = [row for row in events if row["kind"] == "acceptance_plan_saved"]
        acceptance_related_events = [row for row in events if "acceptance" in row["kind"].casefold()]
        prepared_events = [row for row in events if row["kind"] == "provider_attempt_prepared"]
        completed_events = [row for row in events if row["kind"] == "provider_attempt_completed"]
        require_audit(len(acceptance_events) == 1, "acceptance_saved_once")
        require_audit(len(prepared_events) == 1 and len(completed_events) == 1,
                      "provider_events_exactly_once")
        saved_payload = json.loads(acceptance_events[0]["payload_json"])
        prepared_payload = json.loads(prepared_events[0]["payload_json"])
        completed_payload = json.loads(completed_events[0]["payload_json"])
        debited_event = completed_payload.get("attempt_debited")
        debited_event_is_one = (type(debited_event) is int and debited_event == 1) or debited_event is True
        require_audit(saved_payload.get("digest") == plan_row["digest"] and
                      acceptance_events[0]["sequence"] < prepared_events[0]["sequence"] < completed_events[0]["sequence"],
                      "acceptance_frozen_before_dispatch")
        require_audit(all(row["sequence"] < prepared_events[0]["sequence"]
                          for row in acceptance_related_events),
                      "no_acceptance_event_after_dispatch")
        require_audit(
            prepared_payload.get("execution_id") == attempt["execution_id"] and
            prepared_payload.get("client_request_id") == attempt["client_request_id"] and
            prepared_payload.get("provider") == PROVIDER_ID and
            prepared_payload.get("model") == MODEL and
            prepared_payload.get("protocol_family") == FAMILY and
            prepared_payload.get("config_identity") == config_identity and
            isinstance(prepared_payload.get("governor"), dict) and
            completed_payload.get("client_request_id") == attempt["client_request_id"] and
            completed_payload.get("status") == "completed" and
            completed_payload.get("outcome") == "success" and
            completed_payload.get("delivery_state") == "completed" and
            completed_payload.get("request_id") == attempt["request_id"] and
            completed_payload.get("config_identity") == config_identity and
            completed_payload.get("upstream_reached") is True and
            completed_payload.get("uncertain") is False and
            debited_event_is_one,
            "provider_event_ids_and_identity_reconciled",
        )

        usage = db.execute(
            "SELECT attempts,retries FROM governor_task_states WHERE task_id=?", (task_id,),
        ).fetchone()
        ledger_count = db.execute(
            "SELECT count(*) FROM governor_ledger WHERE task_id=?", (task_id,),
        ).fetchone()[0]
        require_audit(usage is not None and usage["attempts"] == 1 and usage["retries"] == 0 and
                      ledger_count == 1, "governor_retry_and_debit_counts")

        zero_tables = (
            "actions", "tool_attempts", "tool_results", "verification_attempts",
            "write_policy_decisions", "approvals",
        )
        counts = {}
        for table in zero_tables:
            counts[table] = db.execute(
                f"SELECT count(*) FROM {table} WHERE task_id=?", (task_id,),
            ).fetchone()[0]
        require_audit(all(count == 0 for count in counts.values()), "zero_task_actions_and_effects")
        return {
            "task_id": EXPECTED_TASK_ID,
            "task_status": task["status"],
            "task_outcome": task["outcome"],
            "resume_count": task["resume_count"],
            "provider_execution_id": attempt["execution_id"],
            "client_request_id": attempt["client_request_id"],
            "upstream_request_id": attempt["request_id"],
            "provider_attempts": len(attempts),
            "admissions": usage["attempts"],
            "debits": ledger_count,
            "retries": usage["retries"],
            "acceptance_saved_before_dispatch": True,
            "provider_event_ids_reconciled": True,
            "actions": counts["actions"],
            "tool_attempts": counts["tool_attempts"],
            "tool_results": counts["tool_results"],
            "verifiers": counts["verification_attempts"],
            "write_decisions": counts["write_policy_decisions"],
            "approvals": counts["approvals"],
            "config_identity_reconciled": True,
            "cli_exit_context": "not_persisted_in_sqlite",
        }
    finally:
        db.close()


def audit_existing(state_dir: Path, task_id: str) -> int:
    """Read existing state only; this path never invokes Runstead or a provider."""
    secret_result = scan_secret(state_dir)
    summary = None
    failed_predicate = ""
    try:
        require_audit(state_dir.is_dir(), "state_directory_present")
        require_audit((state_dir / "runstead.db").is_file(), "state_database_present")
        summary = audit_existing_database(state_dir / "runstead.db", task_id)
    except AuditViolation as exc:
        failed_predicate = exc.predicate
    except SystemExit:
        failed_predicate = "stage2_static_preflight"
    except RuntimeError:
        failed_predicate = "stage2_static_preflight"
    except (OSError, ValueError, KeyError, IndexError, TypeError, sqlite3.Error):
        failed_predicate = "database_or_contract_readable"

    if summary is not None:
        print("db_predicates=pass")
        for key, value in summary.items():
            print(f"{key}={value}")
    else:
        print("db_predicates=fail")
        print(f"failed_predicate={failed_predicate or 'audit_failed'}")
    print(f"secret_scan={secret_result}")
    if secret_result == "present":
        print("audit_result=FAIL")
        print("secret_absence=not_proven")
        return 2
    if summary is None:
        print("audit_result=FAIL")
        print("secret_absence=not_proven" if secret_result != "clean" else "secret_absence=checked_clean")
        return 2
    if secret_result != "clean":
        print("audit_result=LIMITED")
        print("secret_absence=not_proven")
        return 3
    print("audit_result=EVIDENCE_SHAPE_CONFIRMED")
    print("stage2=NOT_QUALIFIED gate_a=NOT_SATISFIED")
    print("secret_absence=checked_clean")
    return 0


def run(args) -> None:
    expected_hash = static_preflight()
    if args.preflight_only:
        print("PREFLIGHT PASS: objective, acceptance, allowed tools, and fixture are semantically aligned")
        print(f"acceptance=file_hash:app/calc.go:sha256:{expected_hash}")
        print("profile=repo.read@1.0.0; task actions limited to read_file/list_files; process-backed tools fail Stage 2")
        print("independent_audit=durable_actions+observations+citation+verifier+governor_reconciliation")
        return

    secret = os.environ.get("GROQ_API_KEY", "")
    if not secret:
        stop("GROQ_API_KEY reference is absent; zero task requests made")
    binary = Path(args.runstead_bin).resolve()
    if not binary.is_file():
        stop("Runstead binary is unavailable; zero task requests made")
    workspace = Path(args.workspace).resolve()
    state_dir = Path(args.state_dir or "/tmp/groq-gpt-oss-v2-stage2-state").resolve()
    if workspace.exists() or state_dir.exists():
        stop("Stage 2 workspace/state path already exists; refusing reuse")
    workspace.parent.mkdir(parents=True, exist_ok=True)
    state_dir.parent.mkdir(parents=True, exist_ok=True)
    shutil.copytree(ROOT / "fixtures/coding-loop", workspace)
    if sha((workspace / "app/calc.go").read_bytes()) != expected_hash:
        stop("fresh workspace differs from the reviewed fixture before dispatch")

    # The no-process boundary applies to task actions and the Runstead control
    # plane as well: without git/rg on this isolated PATH, baseline/verifier
    # observation fails closed without launching a subprocess. The CLI itself
    # remains the harness process required to execute one normal task.
    empty_path = Path(args.empty_path).resolve()
    if not empty_path.is_dir() or any(empty_path.iterdir()):
        stop("isolated empty PATH directory is unavailable or not empty")

    cmd = [
        str(binary), "run", "--task", OBJECTIVE, "--workspace", str(workspace),
        "--providers", str(HERE / "providers.json"), "--provider-id", PROVIDER_ID,
        "--profile", str(HERE / "stage2-profile.json"),
        "--acceptance", str(HERE / "stage2-acceptance.json"),
        "--state-dir", str(state_dir), "--retry-policy", "off", "--log-level", "error",
    ]
    run_env = {"GROQ_API_KEY": secret, "PATH": str(empty_path)}
    try:
        result = subprocess.run(cmd, cwd=ROOT, env=run_env, capture_output=True, text=True,
                                check=False, timeout=300)
    except subprocess.TimeoutExpired:
        # Do not emit the exception: Python includes captured stdout/stderr in
        # its representation, which could contain untrusted provider text.
        print("stage2=NOT_QUALIFIED; CLI timed out; preserve state and do not rerun automatically")
        raise SystemExit(124)
    stdout, stderr = result.stdout, result.stderr
    if secret in stdout or secret in stderr:
        # Never forward a credential if the CLI unexpectedly emitted it.
        stdout = stderr = ""
        if contains_bytes(state_dir, secret.encode("utf-8")):
            stop("secret-output guard fired and secret is present in retained state; raw values discarded")
        stop("secret-output guard fired; captured output was discarded")
    task_match = re.search(r"(?m)^task: (cli-[0-9]+)$", stderr)
    if task_match is None:
        print(f"stage2_cli_exit={result.returncode}")
        print("task_id=none; inspect retained state manually; no rerun authorized")
        raise SystemExit(result.returncode or 1)
    task_id = task_match.group(1)
    if result.returncode != 0:
        print(f"stage2_cli_exit={result.returncode} task_id={task_id}")
        print("stage2=NOT_QUALIFIED; preserve this trajectory and do not rerun automatically")
        raise SystemExit(result.returncode)
    note, cited_ids = final_note(stdout)
    db_path = state_dir / "runstead.db"
    if contains_bytes(state_dir, secret.encode("utf-8")):
        note = stdout = stderr = ""
        stop("secret value found in retained Stage 2 state; raw output and claim discarded")
    if contains_bytes(state_dir, note.encode("utf-8")):
        note = stdout = stderr = ""
        stop("raw final note was persisted; raw output and claim discarded")
    summary = audit_database(db_path, task_id, note, cited_ids, expected_hash)
    print("STAGE2 AUDIT PASS")
    for key, value in summary.items():
        print(f"{key}={value}")
    print("read_file_path=app/calc.go observation_matches_fixture=true final_citation_grounded=true")
    print("write_effects=0 recipe_process_effects=0 terminal=completed verifier=passed")
    print("raw_final_note=discarded raw_stdout=discarded raw_stderr=discarded secret_values=not_emitted")


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--audit-existing", action="store_true",
                        help="read-only audit of an existing Stage 2 state directory; never runs Runstead")
    parser.add_argument("--runstead-bin", default="/tmp/runstead-v2")
    parser.add_argument("--workspace", default="/tmp/groq-gpt-oss-v2-stage2-workspace")
    parser.add_argument("--state-dir", default=None)
    parser.add_argument("--empty-path", default="/tmp/runstead-stage2-empty-path")
    parser.add_argument("--preflight-only", action="store_true")
    parser.add_argument("--task-id", default=None,
                        help="required with --audit-existing; existing task identifier only")
    args = parser.parse_args()
    if args.audit_existing:
        if not args.state_dir or not args.task_id:
            print("audit_result=FAIL")
            print("failed_predicate=audit_existing_requires_state_dir_and_task_id")
            raise SystemExit(2)
        raise SystemExit(audit_existing(Path(args.state_dir), args.task_id))
    if args.task_id is not None:
        print("STAGE2 STOP: --task-id is only valid with --audit-existing", file=sys.stderr)
        raise SystemExit(2)
    try:
        run(args)
    except (OSError, ValueError, KeyError, IndexError, sqlite3.Error, RuntimeError) as exc:
        # Exception messages are authored here from typed failure categories;
        # never print raw provider output, SQL rows, or exception reprs.
        print(f"STAGE2 STOP: {exc}", file=sys.stderr)
        raise SystemExit(2)


if __name__ == "__main__":
    main()
