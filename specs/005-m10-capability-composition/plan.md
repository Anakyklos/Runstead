# Implementation Plan: M10 Capability Composition

**Branch:** `feat/54-m10-capability-composition`

**Issue:** #54

**Spec:** `specs/005-m10-capability-composition/spec.md`

## Scope

Implement only metadata-only built-in CapabilityPackages, strict declarative Profiles, deterministic resolver/canonical hash, frozen task persistence, run/resume/inspect integration, and authority/drift tests. Do not alter the Work Unit scheduler or trusted-kernel semantics.

## Sequence

1. Define and test the strict `internal/composition` API.
2. Implement built-in package registry and deterministic canonical resolver.
3. Add task migration and composition-independent state integrity checks.
4. Wire `run --profile` through existing provider/recipe/tool/governor/policy/verifier seams.
5. Wire exact `resume --profile` validation before recovery and add Work Unit containment.
6. Add deterministic run/inspect/resume E2E, negative authority proofs and drift tests.
7. Update operator documentation and run all repository gates.

## Validation matrix

| Requirement | Evidence |
| --- | --- |
| Strict profile/version/package parsing | `internal/composition` parser tests |
| Deterministic order-independent composition | resolver canonical bytes/hash tests |
| No secrets in material/hash/inspect | contract and CLI redaction tests |
| Pre-dispatch invalid composition failure | CLI tests with zero fake-provider attempts and no task row |
| Atomic authoritative persistence | state migration/integrity tests |
| Resume freezes original contract | CLI drift tests before recovery event/dispatch |
| Existing policy remains authority | write package approval/deny E2E |
| Existing governor remains authority | scripted/provider attempt accounting tests |
| Existing verifier/evidence remain authority | completion and evidence tests |
| Work Unit containment | restricted registry test |
| Race safety and compatibility | package/full `-race` suites |

## Out of scope proof

Review the final diff for absence of dynamic plugin loading, arbitrary executable callbacks, new retry/fallback/routing, scheduler changes, parallel writes, model-created packages and policy/governor/verifier replacement fields.
