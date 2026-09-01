# Capability Packages and Profiles

M10 issue #54 adds a small operator-composition boundary without making the
trusted execution kernel extensible. The implementation lives in
`internal/composition` and uses only metadata compiled into the Runstead binary.

## Profile file

`runstead run --profile FILE` accepts one strict JSON object:

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

`version`, `profile_id`, `profile_version` and at least one exact package
reference are required. Unknown fields, duplicate keys, duplicate references,
unknown package IDs, unknown package versions, missing recipe catalogs and
unknown recipe IDs fail before task bootstrap or provider dispatch. Package and
recipe order is not semantic.

`provider_id`, when present, must agree with the explicitly selected provider.
The Profile does not contain provider credentials, policy, governor, verifier,
approval, executable callback or command fields. A provider endpoint continues
to be resolved by the existing `internal/provider` contract.

## Built-in packages

The initial static registry contains only three packages:

| Package | Existing actions | Boundary |
| --- | --- | --- |
| `repo.read@1.0.0` | `read_file`, `list_files`, `search_text`, `git_status`, `git_diff` | observation evidence and core verifier |
| `repo.write@1.0.0` | `write_file`, `apply_patch` | durable write effect, recovery and existing policy/approval |
| `process.recipes@1.0.0` | `run_recipe` | bounded process effect, recovery and existing policy/approval |

A `CapabilityPackage` is typed metadata. It has an identity, provenance, kind,
version compatibility, action names, declared requirements and bounded effect,
recovery, evidence and verification metadata. It has no function values,
commands, credentials or dynamic loading mechanism. Selecting a write or recipe
package exposes the existing action through a restricted registry view. It does
not authorize the action. Policy and operator approval remain trusted-kernel
responsibilities.

## Deterministic resolution

The resolver validates the Profile and exact package registry references, checks
dependencies/conflicts/runtime compatibility, checks that package actions exist
in the real tool registry, and validates selected recipes against the real
catalog. It then materializes the existing `tools.Registry.Restricted` view.

All order-insensitive slices are sorted before canonicalization. The frozen
contract includes only non-secret identities: Profile identity, package
identity and provenance, effective tool schemas and digest, recipe catalog
identity, sanitized `provider.Identity`, runtime/protocol identity and fixed
trusted-kernel identity markers. The contract hash is SHA-256 over canonical
JSON that excludes the hash itself.

## `recipe_ids` semantics

`recipe_ids` is an EXACT allowlist of the recipes available to the composed
task:

- A non-empty `recipe_ids` restricts the effective recipe surface to exactly
  those ids. A recipe configured in the operator's catalog but absent from the
  selection does not belong to the task surface: the model can propose it, but
  it is rejected with `unknown_recipe` before any process starts, exactly like
  a recipe that never existed.
- Every listed id must exist in the configured catalog; an unknown id fails
  before task bootstrap.
- An EMPTY `recipe_ids` with the `process.recipes` package enabled
  deliberately selects the WHOLE configured catalog. This is intentional and
  regression-tested, so an operator who wants every configured recipe simply
  omits the field.
- `recipe_ids` without a `process.recipes` package records the selection in the
  contract but exposes no `run_recipe` tool, so nothing can execute.

The resolver materializes one effective `recipe.Catalog` containing exactly the
selected recipes. That single catalog is the ONLY recipe surface the runtime
sees: the task registry, the recipe policy the task persists, the frozen
contract's recipe catalog identity (ids + digest over the selection) and its
`recipe_policy_identity` (rendered over the effective ids only, so a mode
assigned to an unselected recipe never enters the frozen identity), resume,
and every Work Unit derived from the task all reference the same effective
catalog. A recipe outside the selection can never reappear through the
original catalog: `executeRunRecipe` resolves against the effective catalog
only, and Work Unit restricted views inherit it. Selecting `process.recipes`
never authorizes a recipe: every `run_recipe` proposal still goes through the
existing policy/approval boundary and the recipe policy renders only the
effective surface. The FULL catalog digest (`recipe_catalog_digest` in the
durable task snapshot) remains the legacy #26 drift identity and is kept
separate from the M10 effective recipe identity.

## Persistence and resume

The contract JSON and its hash are stored together in dedicated task columns by
migration 0015. State validates that both are present together, the JSON is a
strict object with no duplicate keys, and the hash matches the exact bytes.
Legacy tasks keep both columns empty and continue through the legacy path.

A task with a frozen contract must be resumed with the original Profile. Resume
re-resolves the current non-authoritative seams and requires an exact canonical
byte and hash match before the recovery pipeline starts. Changing the Profile,
package version, provider identity, tool schema, recipe catalog, or runtime
contract is rejected. There is no package upgrade, fallback, migration or
silent capability removal. An explicit Profile cannot be attached to a legacy
task during resume.

`runstead inspect TASK` prints the sanitized Profile identity, contract hash,
package identities, provider identity, tool schema identity, recipe catalog
identity and compatibility status. It never prints credentials, auth headers,
cookies, raw provider configuration or model response blobs.

The protected OmniRoute lane is also frozen as a provider identity. Its
`omniroute-config.v1` identity uses the existing `provider.Identity` shape and
the sanitized `provider.Config` representation, with a digest over the
non-secret route inputs and the derived account-lane hash. The API key and raw
connection pin are never part of the contract or hash. A resumed OmniRoute task
must receive the original OmniRoute flags or `OMNIROUTE_*` environment again;
missing or drifted inputs fail before recovery. If recovery finds an interrupted
receipt-aware provider attempt, the existing conservative accounting gate can
block continuation and no second chat request is issued.

## Trusted-kernel boundary

Composition is not an execution engine. The execution path remains:

```text
Profile metadata
  -> existing protocol parser
  -> existing registry/schema validation
  -> existing policy and approval
  -> existing durable effect boundary
  -> existing executor/governor
  -> existing evidence
  -> existing verifier
```

The governor still admits and accounts for every provider attempt. The local
SQLite task/effect/evidence state remains authoritative. Work Units may only use
tools inside the frozen parent registry and continue to use the existing
scheduler. No plugin, `.so`, subprocess capability, WASM, provider fallback,
retry loop or parallel-writer path is introduced by M10.
