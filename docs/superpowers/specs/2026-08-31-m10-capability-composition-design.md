# M10 Capability Composition Design

**Date:** 2026-08-31

**Issue:** #54

**Classification:** Architectural vertical slice

## Decision

Add a small `internal/composition` package that owns declarative metadata and deterministic contract materialization, while the existing execution path remains the only engine. The package registry is compiled-in and metadata-only. A Profile can narrow the existing tool registry and identify existing provider/recipe seams, but no Profile field can replace governor, policy, approvals, durable state, evidence, recovery or verifier.

## Alternatives considered

1. **Put Profile fields directly into `internal/config`.** Rejected because provider configuration and the M10 runtime Profile have different meanings, and this would blur the existing `provider.CapabilityProfile` boundary.
2. **Create a general plugin framework or dynamic loader.** Rejected because it adds a new trust boundary, arbitrary executable code and portability/security cost without a demonstrated requirement. It is explicitly outside #54's base gate.
3. **Use a metadata-only composition boundary above existing registries.** Chosen. It provides repeatable operator composition, deterministic identity and frozen task state without a second executor.

## Boundaries and data flow

```text
strict Profile JSON + built-in package registry
                     |
                     v
      deterministic resolver (no execution/callbacks)
                     |
        effective tools + catalog/provider identities
                     |
                     +--> tools.Registry.Restricted
                     +--> agent loop / existing governor / policy / verifier
                     |
                     v
       canonical non-secret FrozenExecutionContract
                     |
              state task columns + inspect
                     |
              resume exact contract validation
```

The resolver receives an already validated provider identity when one is configured, the real registry's static `Describe()` output and the real recipe catalog. It checks every package action against the actual registered tool schema and every selected recipe against the supplied catalog. It returns an immutable-by-convention contract and an effective registry view. It performs no provider construction, model dispatch, tool execution, persistence or authorization.

## Types

`CapabilityPackage` is a value type with ID, semantic version string, provenance, kind, action names, declared capability/effect/recovery/evidence/verification metadata, bounded-output metadata, approval boundary label, dependencies and conflicts. It has no function fields, interfaces, commands or credential fields.

`Profile` is strict versioned JSON with profile ID/version, optional provider ID, package references and optional recipe IDs. Its wire vocabulary deliberately omits policy, governor, verifier, approval decisions, credentials and executable code. Package references include both ID and version, so an omitted version cannot select a default silently.

`FrozenExecutionContract` contains sorted package identities/metadata, profile identity, sanitized provider identity, sorted effective tool descriptors plus a schema fingerprint, selected recipe IDs plus catalog fingerprint, and runtime/protocol identities. The hash is SHA-256 of the canonical JSON material without the hash field. Slices are sorted and no maps are serialized in the contract.

## Error and drift behavior

Profile parse and resolution errors use stable typed sentinels/codes and happen before provider adapter construction or task bootstrap. Unknown fields and duplicate JSON keys are rejected. Unknown packages, versions, conflicts, duplicate references, invalid tool references and missing recipes are rejected. Hash mismatch, incomplete persisted contract columns, changed profile/package/provider/recipe/runtime material and missing required packages fail closed on resume. Existing tasks with empty M10 contract columns use the existing legacy path.

## Persistence

Migration 0015 adds `execution_contract_json` and `execution_contract_hash` to `tasks`, both empty by default for old tasks. `CreateTask` validates the pair when present and writes it in the same transaction as the task root/event. State does not import composition. It validates the SHA-256 over the exact persisted contract bytes and rejects one-sided/corrupt values. Recovery snapshots and inspect load the columns from the task projection.

## CLI integration

`run --profile FILE` loads Profile before provider dispatch, reconciles its provider ID with `--provider-id`, resolves the existing provider identity, loads existing recipes, resolves the effective contract, restricts the existing registry, and passes the contract bytes/hash into `agent.BootstrapTask` via `state.TaskRecord`. `resume TASK --profile FILE` reconstructs the same inputs and compares canonical bytes/hash before any recovery event. `inspect TASK` renders only the persisted sanitized contract identity and a compatibility status. Existing runs without Profile retain the current configuration snapshot and behavior.

## Negative authority proofs

The test suite will assert that package metadata cannot add an unregistered tool, mark a write approved, alter static policy/governor configuration, change verifier/evidence semantics, or create an effect. A Profile task with the write package still pauses/denies according to the existing policy. A provider E2E continues to count through the existing governor. Tool execution remains a call to the existing Registry. Work Unit restricted views are built from the effective parent registry and cannot include omitted package actions.

## Test strategy

Tests are written first for strict parsing, package/version resolution, order-independent hashes, material drift, secret exclusion, registry containment and state integrity. A deterministic CLI E2E uses scripted provider responses and an acceptance plan to cover `run -> persisted task -> inspect -> interruption/resume`, then mutates Profile/package/catalog/default inputs to prove the task never silently adopts them. Race tests cover the new package and state paths. Full repository and CI gates remain required.
