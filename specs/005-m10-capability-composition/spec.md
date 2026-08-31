# M10 Typed Capability Packages and Declarative Profiles

**Feature branch:** `feat/54-m10-capability-composition`

**Issue:** #54

**Status:** Proposed implementation slice

## Goal

Add the smallest operator-composable M10 vertical slice: strict JSON Profiles select versioned built-in metadata-only CapabilityPackages; a deterministic resolver materializes a non-secret effective execution contract; the contract is persisted with the task before execution, shown by `inspect`, and validated on `resume`.

## Non-goals

This slice does not add Go plugins, shared objects, dynamic code loading, WASM, subprocess capability plugins, model-created packages, package installation, automatic provider selection/fallback, a new execution engine, scheduler changes, parallel writes, policy/governor/verifier replacement, or M11 proposals.

## Architecture

`internal/composition` owns only declarative types, built-in metadata, strict Profile parsing, deterministic resolution and frozen-contract canonicalization. It may inspect existing provider identity, tool descriptions and recipe catalog fingerprints, but it never executes a provider, tool or recipe and contains no callbacks.

The existing agent loop, `internal/tools.Registry`, `internal/policy`, `internal/governor`, `internal/state`, `internal/recovery` and `internal/verifier` remain the trusted execution spine. A resolved Profile restricts the existing registry view; it does not authorize effects. Write and recipe packages declare requirements while the existing policy and approval boundary remains authoritative.

## Profile wire contract

The operator file is strict JSON:

```json
{
  "version": 1,
  "profile_id": "coding-local",
  "profile_version": "1.0.0",
  "provider_id": "local-openai",
  "packages": [
    {"id": "repo.read", "version": "1.0.0"},
    {"id": "repo.write", "version": "1.0.0"}
  ],
  "recipe_ids": ["go-test"]
}
```

Unknown fields, duplicate object keys, unknown versions, unknown packages, missing package versions, duplicate package references, missing referenced recipes, and conflicting package metadata fail before provider dispatch or effects. The only package source in this slice is a registry of built-in/native metadata compiled into the binary.

`provider_id` is optional for scripted runs. If present, it must agree with `--provider-id`; an explicit flag/profile conflict fails rather than using parse order. Profile JSON cannot contain policy, governor, verifier, approval, credential or executable callback fields.

## Built-in package set

The minimal registry contains:

- `repo.read@1.0.0`: existing read-only tools (`read_file`, `list_files`, `search_text`, `git_status`, `git_diff`), read-workspace declaration, observation evidence requirement and core verifier complement.
- `repo.write@1.0.0`: existing `write_file` and `apply_patch`, workspace-write/reconciliation declarations and the existing policy/approval requirement. Metadata never grants approval.
- `process.recipes@1.0.0`: existing `run_recipe`, bounded process/recovery declarations and the existing policy/approval requirement. The configured recipe catalog remains operator input.

No package contains executable function values. Package action names are checked against the real registry description and the effective set is materialized by `Registry.Restricted`.

## Frozen execution contract

The contract contains only canonical, non-secret data:

- contract schema/runtime/protocol identities;
- profile ID/version;
- sorted package IDs, versions, provenance, kind and declared metadata;
- sorted effective tool names and schema fingerprint;
- selected recipe IDs and catalog digest when applicable;
- sanitized `provider.Identity` fields when configured;
- execution contract SHA-256 over canonical material excluding the hash field.

The serialized contract is stored in dedicated task columns alongside the hash. State validates that the stored bytes and hash agree. A missing or mismatched pair is corruption and fails closed. Existing tasks with no contract remain compatible with the pre-M10 path.

## Resume and drift

A Profile run stores its frozen contract before the first model/provider attempt. Resume of such a task requires the same Profile material and the existing provider/recipe inputs needed to reconstruct its boundaries. The newly resolved contract must byte-match the persisted contract and hash. A changed Profile, package version, provider identity, recipe catalog or runtime/protocol identity is rejected. Missing packages or recipes never fall back or migrate automatically. A task without a frozen M10 contract continues through the legacy resume path.

## Trusted-kernel proofs

Tests must show that unknown package/version, duplicate/conflicting composition, and invalid contract data fail pre-dispatch; package metadata cannot alter governor, policy/approval, evidence semantics or verifier behavior; a write package still reaches the existing policy gate; provider attempts still use the existing governor; and tool execution still uses the existing registry. Work Unit restricted views may only be derived from the task's effective registry and cannot add actions outside the frozen tool set.

## CLI vertical slice

`run --profile FILE` loads and validates the Profile before execution and uses its effective registry. `resume TASK --profile FILE` validates the same contract before recovery. `inspect TASK` prints sanitized Profile identity/version, contract hash, package identities, provider identity, tool schema identity and recipe catalog identity/compatibility without secrets.

Existing invocations without `--profile` preserve their behavior and do not change the default provider, governor, policy, verifier, work-unit scheduler or retry semantics.

## Acceptance evidence

The implementation is accepted only when a deterministic scripted E2E covers `run -> persist -> inspect -> resume`, profile/package/catalog drift, hash corruption, order-independent canonicalization, material hash changes, provider/governor/tool/policy/verifier containment, secret exclusion, race safety, the full repository suite and the existing CI quality gates.
