#!/usr/bin/env python3
"""Deterministic, offline objective/acceptance/tool/fixture preflight for Stage 2."""
import hashlib
import json
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
    if profile.get("provider_id") != EXPECTED_PROVIDER["provider_id"]:
        fail("read-only profile is not pinned to the exact candidate")
    if packages != [{"id": "repo.read", "version": "1.0.0"}] or profile.get("recipe_ids"):
        fail("profile exposes a write or process package")
    if set(contract.get("allowed_tools", [])) != READ_ONLY_TOOLS:
        fail("declared tools differ from the built-in read-only package")
    if contract.get("prohibited_effects") != [
        "file writes", "patches", "recipe execution", "process execution", "network access from tools"
    ]:
        fail("effect boundary does not exclude writes and process execution")

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

    required = set(contract.get("required_evidence", []))
    if len(required) != 5 or not any("app/calc.go" in item for item in required):
        fail("independent post-run evidence contract is incomplete")
    if not any("final evidence citation" in item for item in required):
        fail("post-run contract does not bind the final claim to cited evidence")
    if not any("no write_file, apply_patch, or run_recipe" in item for item in required):
        fail("post-run contract does not prohibit write/process effects")

    print("PREFLIGHT PASS: objective, acceptance, read-only tool surface, and fixture are aligned")
    print(f"provider_id={EXPECTED_PROVIDER['provider_id']} family=openai_compatible model={EXPECTED_PROVIDER['model']}")
    print(f"stage2_acceptance=file_hash:app/calc.go:sha256:{digest}")
    print("allowed_tools=read_file,list_files,search_text,git_status,git_diff")
    print("write_effects=prohibited recipe_process_effects=prohibited")
    print("independent_post_run_audit=required")


if __name__ == "__main__":
    main()
