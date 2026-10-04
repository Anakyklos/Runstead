#!/usr/bin/env python3
"""Deterministic, offline objective/acceptance/tool/fixture preflight for Stage 2."""
import hashlib
import json
import runpy
from pathlib import Path

ROOT = Path(__file__).resolve().parents[3]
HERE = Path(__file__).resolve().parent
OBJECTIVE = (
    "Read app/calc.go and identify the whitespace-handling bug in ParseValues. "
    "Do not modify files and do not run recipes/processes. "
    "Base the final claim on the actual read_file evidence."
)
EXPECTED_PROVIDER = {
    "provider_id": "groq-gpt-oss-120b-canary-v2",
    "protocol_family": "openai_compatible",
    "base_url": "https://api.groq.com/openai/v1",
    "model": "openai/gpt-oss-120b",
    "auth_requirement": "reference_required",
    "auth_ref": "GROQ_API_KEY",
}
READ_ONLY_TOOLS = {"read_file", "list_files", "search_text", "git_status", "git_diff"}


def fail(reason: str) -> None:
    raise SystemExit(f"PREFLIGHT STOP: {reason}; zero provider requests permitted")


def load(name: str):
    try:
        return json.loads((HERE / name).read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        fail(f"cannot read {name}: {type(exc).__name__}")


def main() -> None:
    provider_doc = load("providers.json")
    if provider_doc.get("version") != 1 or len(provider_doc.get("providers", [])) != 1:
        fail("provider declaration is not one exact v1 candidate")
    provider = provider_doc["providers"][0]
    for key, expected in EXPECTED_PROVIDER.items():
        if provider.get(key) != expected:
            fail(f"candidate {key} differs from the issue contract")
    safety = provider.get("profile", {}).get("route_safety", {})
    if any(safety.get(k) != "disabled" for k in (
        "internal_retries", "cooldown_replay", "account_pooling",
        "automatic_fallback", "combo_routing",
    )) or safety.get("attempt_accounting") != "single" or safety.get("single_attempt") != "guaranteed":
        fail("provider route safety does not prove one non-amplified attempt")

    contract = load("stage2-preflight.json")
    profile = load(contract["profile"])
    packages = profile.get("packages", [])
    if contract.get("objective") != OBJECTIVE:
        fail("objective differs from the reviewed read-only objective")
    if contract.get("workspace") != "fresh temporary copy of fixtures/coding-loop; do not initialize Git so verifier Git observation launches no process":
        fail("workspace setup does not preserve the no-process boundary")
    if profile.get("provider_id") != EXPECTED_PROVIDER["provider_id"]:
        fail("read-only profile is not pinned to the exact candidate")
    if packages != [{"id": "repo.read", "version": "1.0.0"}] or profile.get("recipe_ids"):
        fail("profile exposes a write or process package")
    if set(contract.get("allowed_tools", [])) != READ_ONLY_TOOLS:
        fail("declared tools differ from the built-in read-only package")
    if contract.get("prohibited_effects") != [
        "file writes", "patches", "recipe execution", "process-backed search/Git tools", "network access from tools"
    ]:
        fail("effect boundary does not exclude writes and process execution")
    if contract.get("process_boundary") != {
        "task_actions": "no process-backed tools or recipes; only read_file/list_files may be attempted",
        "control_plane": "Git and ripgrep are intentionally unavailable on the isolated task PATH, so bootstrap/verifier and observation tools cannot launch subprocesses",
        "qualification": "any search_text, git_status, git_diff, run_recipe, write_file, or apply_patch action/attempt fails Stage 2",
    }:
        fail("task/control-plane process boundary is ambiguous or unenforced")

    fixture = ROOT / "fixtures/coding-loop"
    source = (fixture / "app/calc.go").read_bytes()
    test_source = (fixture / "app/calc_test.go").read_text(encoding="utf-8")
    digest = hashlib.sha256(source).hexdigest()
    plan = load(contract["acceptance_plan"])
    checks = plan.get("checks", [])
    expected_check = {
        "id": "calc-source-unchanged", "type": "file_hash",
        "path": "app/calc.go", "sha256": digest,
    }
    if checks != [expected_check] or contract.get("acceptance_checks") != checks:
        fail("acceptance does not bind the objective fixture file to its baseline hash")
    text = source.decode("utf-8")
    parse_values = text.split("func ParseValues(", 1)[1].split("\n}", 1)[0]
    if "strings.TrimSpace(" in parse_values or "strconv.Atoi(part)" not in parse_values:
        fail("fixture no longer contains the ParseValues whitespace bug")
    if 'ParseValues("1, 2 , 3")' not in test_source:
        fail("fixture test no longer exercises ParseValues whitespace handling")

    post = contract.get("post_run_verification", {})
    if post.get("required_action") != {"tool": "read_file", "path": "app/calc.go", "success": True}:
        fail("independent audit does not require the target read_file action")
    if post.get("required_observation") != {
        "data_path": "app/calc.go", "metadata_path": "app/calc.go", "sha256_matches_fixture": True
    }:
        fail("independent audit does not require target observation and matching content")
    if post.get("final_claim") != {
        "requires": [
            "bug or defect assertion", "ParseValues", "strconv.Atoi(part)", "positive no-trim statement",
            "positive whitespace parse-failure statement",
        ],
        "single_sentence_full_match": True,
        "rejects_contradiction_markers": ["not", "never", "but", "however", "actually", "except", "unless", "although"],
        "citation_must_match_observation": True,
    }:
        fail("independent audit does not bind a grounded claim to the target observation")
    if post.get("prohibited_effect_tools") != ["apply_patch", "run_recipe", "write_file"]:
        fail("independent audit does not reject write/process effects")
    if post.get("process_backed_tools") != ["git_diff", "git_status", "search_text"]:
        fail("independent audit does not reject process-backed observation tools")
    if post.get("terminal") != {
        "task_status": "completed", "latest_verifier": "passed", "acceptance_check": "calc-source-unchanged"
    }:
        fail("independent audit does not require terminal verifier PASS")
    if post.get("attempt_accounting") != {
        "physical_requests_equal_admissions": True, "debits_equal_admissions": True, "retries": 0
    }:
        fail("independent audit does not reconcile physical requests and admissions")
    auditor = HERE / contract.get("independent_audit_runner", "")
    if not auditor.is_file() or "def audit_database(" not in auditor.read_text(encoding="utf-8"):
        fail("independent post-run database verifier is missing")
    claim_template = runpy.run_path(str(auditor)).get("CLAIM_TEMPLATE")
    valid_claim = (
        "The bug in ParseValues is that each part goes to strconv.Atoi(part) without trimming whitespace, "
        "so whitespace-bearing values fail to parse."
    )
    contradictory_claim = valid_claim + " That is not actually a bug; Atoi accepts it."
    if claim_template is None or not claim_template.fullmatch(valid_claim.casefold()) or claim_template.fullmatch(contradictory_claim.casefold()):
        fail("independent claim validator does not accept a positive bug statement and reject contradiction")

    print("PREFLIGHT PASS: objective, acceptance, read-only tool surface, and fixture are aligned")
    print(f"provider_id={EXPECTED_PROVIDER['provider_id']} family=openai_compatible model={EXPECTED_PROVIDER['model']}")
    print(f"stage2_acceptance=file_hash:app/calc.go:sha256:{digest}")
    print("profile_tools=read_file,list_files,search_text,git_status,git_diff")
    print("qualifying_actions=read_file,list_files process_backed_tools=prohibited")
    print("independent_post_run_audit=required")


if __name__ == "__main__":
    main()
