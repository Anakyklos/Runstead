# M10 Capability Composition Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Deliver issue #54's typed built-in CapabilityPackages, strict declarative Profiles, deterministic effective composition, persisted FrozenExecutionContract, and run/resume/inspect vertical slice without changing trusted-kernel authority.

**Architecture:** A metadata-only `internal/composition` package parses Profiles, resolves built-in package references against existing tool/recipe/provider seams, sorts canonical material and produces a SHA-256 contract plus an existing restricted `tools.Registry`. `internal/state` stores exact contract bytes and hash in task-owned columns, while `cmd/runstead` wires `--profile` into the existing agent/governor/policy/verifier path and validates the same contract on resume.

**Tech Stack:** Go standard library, existing `internal/provider`, `internal/tools`, `internal/recipe`, `internal/agent`, `internal/state`, SQLite embedded migrations, existing scripted provider and CLI tests.

**Spec:** `specs/005-m10-capability-composition/spec.md`

## Global Constraints

- Keep the trusted kernel authoritative: governor, policy/approvals, durable task/effect truth, evidence, recovery, verifier, protocol and redaction are not replaceable through Profile metadata.
- Do not add Go plugins, dynamic loading, WASM, arbitrary subprocess capability plugins, model-created packages, a second execution engine, automatic provider/fallback/retry, scheduler changes or parallel writes.
- Keep `provider.CapabilityProfile` separate from the M10 runtime `Profile`.
- Use strict JSON with unknown-field and duplicate-key rejection; fail before provider dispatch/effects.
- Canonical contract material must contain no secrets or raw credentials and must not depend on map iteration order.
- Existing invocations without `--profile` remain behaviorally compatible.
- Every implementation change follows a test-first red-green cycle and ends with focused tests before moving on.

---

### Task 1: Register the implementation slice and establish package API tests

**Files:**
- Create: `specs/005-m10-capability-composition/spec.md`
- Create: `specs/005-m10-capability-composition/plan.md`
- Create: `specs/005-m10-capability-composition/tasks.md`
- Create: `docs/superpowers/specs/2026-08-31-m10-capability-composition-design.md`
- Create: `docs/superpowers/plans/2026-08-31-m10-capability-composition.md`
- Create: `internal/composition/composition_test.go`

**Interfaces:**
- Produces the test names and public API shape used by later tasks: `Profile`, `PackageRef`, `CapabilityPackage`, `PackageRegistry`, `NewBuiltinRegistry`, `ParseProfile`, `ResolveInput`, `Resolve`, `FrozenExecutionContract`, `ContractBytes` and `ValidateContract`.

- [ ] **Step 1: Write failing API/contract tests**

```go
func TestParseProfileRejectsUnknownFieldsAndDuplicateKeys(t *testing.T) {}
func TestResolveBuiltinsProducesSortedEffectiveToolsAndStableHash(t *testing.T) {}
func TestResolveRejectsUnknownPackageAndVersionBeforeExecution(t *testing.T) {}
func TestResolveRejectsDuplicateAndConflictingPackages(t *testing.T) {}
func TestContractHashExcludesProviderSecretsAndChangesOnMaterialInput(t *testing.T) {}
```

The tests should construct only metadata and a real `tools.Registry` over `t.TempDir()`, and must assert typed errors without invoking a provider or tool.

- [ ] **Step 2: Run the focused tests to verify the intended missing API failure**

Run: `go test ./internal/composition -run 'Test(ParseProfileRejectsUnknownFieldsAndDuplicateKeys|ResolveBuiltinsProducesSortedEffectiveToolsAndStableHash|ResolveRejectsUnknownPackageAndVersionBeforeExecution|ResolveRejectsDuplicateAndConflictingPackages|ContractHashExcludesProviderSecretsAndChangesOnMaterialInput)'`

Expected: FAIL because the new package/API does not exist yet.

- [ ] **Step 3: Commit the slice registration and red tests**

```bash
git add specs/005-m10-capability-composition docs/superpowers/specs/2026-08-31-m10-capability-composition-design.md docs/superpowers/plans/2026-08-31-m10-capability-composition.md internal/composition/composition_test.go
git commit -m "test(composition): define M10 package and contract API"
```

---

### Task 2: Implement strict Profile parsing and metadata-only built-ins

**Files:**
- Create: `internal/composition/profile.go`
- Create: `internal/composition/packages.go`
- Create: `internal/composition/json_strict.go`
- Test: `internal/composition/composition_test.go`

**Interfaces:**
- Consumes: the tests from Task 1 and existing tool constants/specifications, `recipe.Catalog` identities and `provider.Identity` values.
- Produces: `const ProfileSchemaVersion = 1`, `const ContractSchemaVersion = 1`, `type Profile`, `type PackageRef`, `type CapabilityPackage`, `type PackageRegistry`, `func NewBuiltinRegistry() PackageRegistry`, `func (r PackageRegistry) Lookup(id, version string) (CapabilityPackage, bool)`, `func ParseProfile(data []byte) (Profile, error)`, `func LoadProfile(path string) (Profile, error)`, and typed errors `ErrInvalidProfile`, `ErrUnknownPackage`, `ErrUnknownPackageVersion`, `ErrDuplicatePackage`, `ErrCompositionConflict`.

- [ ] **Step 1: Make the strict parser tests fail for the expected semantic reasons**

The tests must include an unknown JSON key, a duplicate object key, version `2`, a package reference with an empty version, and an extra `policy` field. Assert errors wrap `ErrInvalidProfile` and never echo secret-like values supplied in rejected fields.

- [ ] **Step 2: Implement Profile and package value types**

Use JSON fields exactly as the spec: `version`, `profile_id`, `profile_version`, `provider_id`, `packages`, `recipe_ids`. Package references contain `id` and `version`. CapabilityPackage contains only strings, sorted string slices, booleans/integers and package references. It must contain no function fields, commands, credential fields or policy/governor/verifier replacement fields.

Built-ins:

```go
repo.read@1.0.0        -> read_file, list_files, search_text, git_status, git_diff
repo.write@1.0.0       -> write_file, apply_patch
process.recipes@1.0.0  -> run_recipe
```

Use a stable provenance such as `runstead/builtin`, kind `builtin`, and metadata values that describe requirements rather than grant authority. The write and recipe packages must state the existing policy/approval boundary, not an approval decision.

- [ ] **Step 3: Implement duplicate-key scanning and strict single-document decoding**

Walk JSON tokens recursively with a depth bound, reject duplicate object keys, use `json.Decoder.DisallowUnknownFields`, reject trailing JSON, validate schema version, non-empty IDs/versions and package reference shape. Normalize only whitespace that has no semantic meaning; never fill a missing package version from a default.

- [ ] **Step 4: Run focused tests and package tests**

Run: `gofmt -w internal/composition && go test ./internal/composition`

Expected: PASS for strict parse tests and built-in metadata tests.

- [ ] **Step 5: Commit**

```bash
git add internal/composition
git commit -m "feat(composition): add strict profiles and built-in packages"
```

---

### Task 3: Implement deterministic resolver and canonical FrozenExecutionContract

**Files:**
- Create: `internal/composition/resolve.go`
- Create: `internal/composition/contract.go`
- Modify: `internal/tools/describe.go` only if a narrow exported schema helper is required
- Test: `internal/composition/composition_test.go`

**Interfaces:**
- Consumes: Task 2 metadata, `tools.Registry.Describe`, `tools.Registry.Restricted`, `recipe.Catalog.IDs/Get/Digest`, `provider.Identity`.
- Produces:

```go
type ResolveInput struct {
    Profile         Profile
    Provider        provider.Identity
    Registry        *tools.Registry
    Recipes         *recipe.Catalog
    RuntimeIdentity string
    ProtocolIdentity string
}

type Resolved struct {
    Contract        FrozenExecutionContract
    ContractJSON    []byte
    ContractHash    string
    EffectiveRegistry *tools.Registry
}

func Resolve(input ResolveInput) (Resolved, error)
func (c FrozenExecutionContract) ContractBytes() ([]byte, string, error)
func ValidateContract(data []byte, hash string) (FrozenExecutionContract, error)
func (c FrozenExecutionContract) ToolNames() []string
```

- [ ] **Step 1: Extend tests with deterministic material and containment cases**

Assert package order permutations produce identical bytes/hash, material package/tool/recipe/provider changes alter hash, effective tools are the union of package actions sorted, package actions absent from the actual registry fail, a recipe package without a catalog fails, selected missing recipes fail, and `EffectiveRegistry.Allows` cannot execute a tool not selected by the package set.

- [ ] **Step 2: Implement resolution validation**

Resolve package references in input order only for diagnostics, reject duplicates, look up exact ID+version, validate static dependencies/conflicts, union actions, reject unknown action names against `registry.Describe`, require recipes for `run_recipe`/selected recipe IDs, and reject a non-empty Profile provider ID that differs from the supplied provider identity. Do not inspect or alter policy/governor/verifier settings.

- [ ] **Step 3: Implement canonical contract structs and hashing**

Use structs and sorted slices, never map fields, for canonical JSON. Include contract schema/runtime/protocol identities, Profile identity, package identities and metadata, provider identity with only sanitized fields (`ProviderID`, family, model, config identity, profile/adapter versions), effective tool descriptors and schema SHA-256, selected recipe IDs and catalog digest. Exclude `Auth`, credentials, endpoints beyond the existing sanitized identity, raw recipe definitions and tool output. Hash exactly the canonical material bytes and prefix it with `sha256:`.

- [ ] **Step 4: Implement contract validation**

Decode strict canonical contract JSON, reject unknown fields/trailing data, require non-empty hash and exact recomputed SHA-256. Re-encode and require byte identity so reordered/tampered material fails closed. Return `ErrInvalidContract` for malformed or inconsistent content.

- [ ] **Step 5: Run resolver tests**

Run: `gofmt -w internal/composition && go test ./internal/composition -count=1`

Expected: PASS, including order independence, hash drift and registry containment.

- [ ] **Step 6: Commit**

```bash
git add internal/composition internal/tools/describe.go
git commit -m "feat(composition): resolve deterministic frozen contracts"
```

---

### Task 4: Persist contract bytes/hash in authoritative task state

**Files:**
- Create: `internal/state/migrations/0015_execution_contract.sql`
- Modify: `internal/state/store.go`
- Modify: `internal/state/tasks.go`
- Modify: `internal/state/recovery.go`
- Modify: `internal/state/inspect.go`
- Test: `internal/state/execution_contract_test.go`
- Test: `internal/state/migrations_test.go` if migration count assertions require update

**Interfaces:**
- Consumes: exact bytes/hash from `composition.Resolved`, but state remains composition-independent.
- Produces:

```go
type TaskRecord struct {
    ...
    ExecutionContractJSON []byte
    ExecutionContractHash string
}

type RecoveryTask struct {
    ...
    ExecutionContractJSON string
    ExecutionContractHash string
}

func (s *Store) LoadExecutionContract(ctx context.Context, taskID string) ([]byte, string, error)
```

- [ ] **Step 1: Write failing state tests**

Cover migration columns, atomic CreateTask persistence, exact load, one-sided pair rejection, hash mismatch rejection, malformed bytes rejection, old task compatibility with both columns empty, and `RenderInspect` output containing only contract identity fields.

- [ ] **Step 2: Run tests to verify missing columns/API**

Run: `go test ./internal/state -run 'Test(ExecutionContract|Migration)'`

Expected: FAIL because migration/API are absent.

- [ ] **Step 3: Add migration 0015**

Add `execution_contract_json TEXT NOT NULL DEFAULT ''` and `execution_contract_hash TEXT NOT NULL DEFAULT ''` to `tasks`. Do not modify prior migrations.

- [ ] **Step 4: Add state-side integrity validation**

In `CreateTask`, require both fields empty or both non-empty. When non-empty, verify the hash is `sha256:` plus 64 lowercase hex characters and equals SHA-256 of the exact JSON bytes. Decode one strict JSON object and reject malformed/trailing bytes. Store both in the same task-root transaction. `LoadExecutionContract` repeats these checks and returns a typed corruption error. Do not add a dependency from state to composition.

- [ ] **Step 5: Carry contract into recovery and inspect projections**

Select the two columns in recovery/inspect task queries. Render a stable `Execution contract:` section with hash and sanitized stored JSON-derived identity fields, or `(none recorded)` for legacy tasks. Never print raw contract blobs, credentials or provider secret references.

- [ ] **Step 6: Run state tests**

Run: `gofmt -w internal/state && go test ./internal/state -count=1`

Expected: PASS, including migration and corruption cases.

- [ ] **Step 7: Commit**

```bash
git add internal/state
git commit -m "feat(state): persist and validate frozen execution contracts"
```

---

### Task 5: Integrate `run --profile` with existing provider, tool, policy and verifier seams

**Files:**
- Modify: `cmd/runstead/main.go`
- Modify: `cmd/runstead/workunit.go`
- Modify: `internal/agent/loop.go` only if TaskRecord plumbing requires no behavior change
- Create: `cmd/runstead/composition_helpers.go`
- Test: `cmd/runstead/composition_e2e_test.go`

**Interfaces:**
- Consumes: `composition.LoadProfile`, `composition.Resolve`, `state.TaskRecord` contract fields, existing provider/config/recipe/policy/governor constructors.
- Produces: `--profile FILE` on `run`, a deterministic profile/provider conflict error, effective registry wiring, and persisted contract before `agent.Loop.Run` or Work Unit bootstrap.

- [ ] **Step 1: Write failing CLI tests**

Create a scripted fake-provider E2E with a Profile selecting `repo.read`, an acceptance plan, and a read action. Assert `run --profile` completes, the task row has non-empty contract bytes/hash, `inspect` prints profile/contract/package/tool identities and no secret fixture value. Add tests for unknown package/version and Profile `provider_id` conflict proving zero provider attempts and no task side effect.

- [ ] **Step 2: Run the E2E tests to verify the absent flag/behavior**

Run: `go test ./cmd/runstead -run 'Test(Composition|Profile)' -count=1`

Expected: FAIL because `--profile` is not parsed/wired.

- [ ] **Step 3: Parse and load Profile before dispatch**

Add `profileFile` and `--profile` to `run`. Resolve the flag/env path with explicit precedence. Load Profile and validate its schema before provider construction. If Profile has `provider_id`, require exact equality with `--provider-id` when both are supplied; use the Profile provider ID when only it is supplied. Keep existing scripted/provider/OmniRoute exclusivity checks and reject a Profile provider ID that cannot be resolved rather than choosing a default.

- [ ] **Step 4: Resolve existing provider and recipe seams, then compose**

After existing provider identity resolution and recipe catalog loading but before adapter construction or any governor Execute call, create the full registry, call `composition.Resolve`, and replace only the agent-facing registry with `resolved.EffectiveRegistry`. Keep the original provider identity, governor, policy, verifier, acceptance plan and recipe policy constructors. The resolver must not create clients, call providers, approve effects or modify limits.

- [ ] **Step 5: Persist contract during every task bootstrap path**

Pass `ExecutionContractJSON` and `ExecutionContractHash` into `state.TaskRecord` for normal and Work Unit root bootstrap. Ensure the contract is persisted before `StartTask`, baseline capture, Work Unit execution or parent loop model dispatch. Legacy no-profile paths leave the fields empty and follow existing behavior.

- [ ] **Step 6: Verify containment and policy authority in CLI tests**

Use a Profile selecting `repo.write` with `write_file` policy `approval_required`; scripted model output proposes a write and assert the existing approval pause occurs. Assert no metadata field can configure a governor limit, policy mode, verifier result or evidence ID. Assert a Profile-selected missing tool is rejected before provider attempt.

- [ ] **Step 7: Run focused CLI tests**

Run: `gofmt -w cmd/runstead internal/agent && go test ./cmd/runstead -run 'Test(Composition|Profile)' -count=1`

Expected: PASS, with provider/governor/tool/policy/verifier still using existing paths.

- [ ] **Step 8: Commit**

```bash
git add cmd/runstead internal/agent/loop.go
 git commit -m "feat(cli): run tasks under frozen declarative profiles"
```

---

### Task 6: Integrate exact contract validation into resume and Work Unit containment

**Files:**
- Modify: `cmd/runstead/resume.go`
- Modify: `cmd/runstead/main.go` inspect/help text
- Modify: `cmd/runstead/workunit.go` only for effective registry containment if required
- Test: `cmd/runstead/composition_e2e_test.go`
- Test: `internal/workunit/composition_containment_test.go`

**Interfaces:**
- Consumes: persisted `RecoveryTask.ExecutionContractJSON/Hash`, Profile resolver, existing resume provider/catalog drift checks.
- Produces: `resume TASK --profile FILE` exact contract validation before recovery, explicit drift/missing-component errors, and Work Unit views that cannot exceed the frozen effective tool set.

- [ ] **Step 1: Write failing resume/drift tests**

Cover Profile A run followed by Profile B file contents, package version changes, removed required package, provider default/model/config changes, recipe catalog digest changes, corrupted hash/JSON, reordered package list and an attempted Work Unit tool outside the frozen package set. Assert A resumes only when its exact material is re-supplied; all drift/corruption cases fail before `MarkRecoveryStarted`, provider dispatch or effect.

- [ ] **Step 2: Run focused tests to observe missing resume enforcement**

Run: `go test ./cmd/runstead ./internal/workunit -run 'Test(Composition|Profile|WorkUnit.*Containment)' -count=1`

Expected: FAIL because resume currently has no M10 contract comparison.

- [ ] **Step 3: Add `--profile` parsing to resume**

Accept the flag before/after task ID using the existing manual parser. Load Profile after the task snapshot is preloaded but before recovery journaling. A task with a persisted M10 contract requires `--profile`; a supplied Profile on a legacy task is rejected explicitly rather than silently changing its configuration.

- [ ] **Step 4: Reconstruct current composition and compare exact bytes/hash**

Resolve the same provider identity, recipe catalog, registry and runtime/protocol identities needed by the persisted contract. Call `composition.Resolve`, then compare both canonical bytes and hash with the persisted values. Validate the persisted pair first. Return a typed/fail-closed error for mismatch, missing package/recipe, provider/profile conflict or unavailable boundary. Perform this before `MarkRecoveryStarted`, acceptance-plan attachment, operational-profile writes or provider client dispatch.

- [ ] **Step 5: Reuse the effective registry for resumed loop and Work Units**

Construct the resumed agent loop, verifier and Work Unit driver with the exact effective registry produced by the validated contract. Keep Work Unit `Restricted` as a second containment layer. Add a check that a unit tool not present in the frozen effective registry is rejected with existing capability containment errors.

- [ ] **Step 6: Run deterministic resume and containment tests**

Run: `gofmt -w cmd/runstead internal/workunit && go test ./cmd/runstead ./internal/workunit -run 'Test(Composition|Profile|WorkUnit.*Containment)' -count=5`

Expected: PASS, with no recovery/provider/effect activity on rejected drift paths.

- [ ] **Step 7: Commit**

```bash
git add cmd/runstead internal/workunit
git commit -m "feat(composition): enforce frozen contract on resume"
```

---

### Task 7: Complete inspect output, documentation and SpecKit tracking

**Files:**
- Modify: `cmd/runstead/main.go`
- Modify: `docs/architecture.md`
- Modify: `docs/roadmap.md`
- Modify: `README.md` only if the operator surface requires a material update
- Modify: `specs/005-m10-capability-composition/spec.md`
- Modify: `specs/005-m10-capability-composition/tasks.md`
- Test: `cmd/runstead/inspect_test.go` or `cmd/runstead/composition_e2e_test.go`

**Interfaces:**
- Consumes: final inspect renderer and CLI behavior from Tasks 4–6.
- Produces: stable operator help, architecture/roadmap statements that M8/M9 are complete and M10/#54 is active while M11/#55 remains blocked, and completed SpecKit evidence.

- [ ] **Step 1: Write failing documentation/inspect assertions**

Assert help mentions `--profile`, inspect prints profile identity/version, contract hash, package ID/version/provenance, sanitized provider identity, tool schema identity and recipe catalog identity/compatibility, and never prints a fixture secret or raw credential value.

- [ ] **Step 2: Implement stable output and narrow docs updates**

Keep fixed labels and deterministic order. Document that M10 Profiles compose non-authoritative seams only, packages are built-in metadata, and the trusted kernel is not pluggable. Update roadmap only for milestone state and sequencing, without rewriting unrelated historical content.

- [ ] **Step 3: Run focused output tests**

Run: `gofmt -w cmd/runstead && go test ./cmd/runstead -run 'Test(Inspect|Composition|Profile)' -count=1`

Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add README.md docs/architecture.md docs/roadmap.md specs/005-m10-capability-composition cmd/runstead
 git commit -m "docs(composition): document M10 profile contract and boundaries"
```

---

### Task 8: Full verification, security review and single PR delivery

**Files:**
- Modify only files already listed above if verification finds a directly related defect
- Test: all relevant package tests and repository gates

**Interfaces:**
- Consumes: complete M10 vertical slice.
- Produces: validated branch, one PR against `main`, complete PR description and explicit remaining risks.

- [ ] **Step 1: Run focused deterministic repetition**

```bash
go test ./internal/composition ./internal/state ./internal/workunit -count=20
go test ./cmd/runstead -run 'Test(Composition|Profile|Inspect)' -count=20
go test -race ./internal/composition ./internal/state ./internal/workunit ./cmd/runstead
```

Expected: all green with no races, leaks, timing assertions or live endpoints.

- [ ] **Step 2: Run repository validation**

```bash
test -z "$(gofmt -l .)"
go test ./...
go vet ./...
go build ./cmd/runstead
go test -race ./...
bash experiments/protocol/test.sh
git diff --check
```

Also run the existing provider-abstraction and quality gates from `.github/workflows/ci.yml`, including protocol golden corpus, quality gate build/self-tests/growth/errcheck/live-convention, sidecar checks and standalone deterministic checks.

- [ ] **Step 3: Inspect final diff for forbidden mechanisms**

```bash
git diff origin/main...HEAD --check
git diff origin/main...HEAD -- README.md docs/architecture.md docs/roadmap.md internal/composition internal/state cmd/runstead internal/workunit specs/005-m10-capability-composition
grep -RInE 'plugin\.Open|\.so|wasm|WASI|python|javascript|subprocess|retry|fallback|governor|verifier|approval' internal/composition specs/005-m10-capability-composition docs/superpowers/specs/2026-08-31-m10-capability-composition-design.md
```

Review every match and confirm it is a prohibition, existing boundary reference or test, never new authority or execution behavior.

- [ ] **Step 4: Commit any directly related verification correction**

```bash
git add <only-related-files>
git commit -m "test(composition): harden M10 acceptance evidence"
```

- [ ] **Step 5: Push and open exactly one PR**

```bash
git push -u origin feat/54-m10-capability-composition
gh pr create --repo Anakyklos/Runstead --base main --head Anakyklos:feat/54-m10-capability-composition --title "feat(composition): add typed capability packages and frozen profiles (#54)" --body-file <validated-body-file>
```

The body must list architecture, non-pluggable kernel boundary, Profile/package contract, canonicalization/hash semantics, migration/persistence, resume drift behavior, negative authority tests, deterministic E2E, all validation results, limitations and explicitly excluded mechanisms. Report the final head SHA, PR URL and any acceptance criterion not proven. Do not merge/close issues/start #55.
