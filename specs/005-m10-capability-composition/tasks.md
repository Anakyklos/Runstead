# Tasks: M10 Typed Capability Packages and Declarative Profiles

**Feature:** `005-m10-capability-composition`

**Issue:** #54

- [x] T001 Add strict Profile and metadata-only built-in package types with duplicate-key and unknown-field rejection.
- [x] T002 Add deterministic resolver that validates exact package versions, dependencies/conflicts, tool references, selected recipes and provider identity, then materializes a restricted existing tool registry.
- [x] T003 Add canonical non-secret FrozenExecutionContract with sorted identities, tool schema/catalog fingerprints and SHA-256 validation.
- [x] T004 Add migration 0015 and state APIs for atomic task persistence, integrity validation, recovery loading and sanitized inspect rendering.
- [x] T005 Add `run --profile FILE` composition-root wiring without changing governor, policy, verifier, provider or tool execution boundaries.
- [x] T006 Add `resume TASK --profile FILE` exact contract reconstruction and fail-closed drift/missing-component handling before recovery or dispatch.
- [x] T007 Add Work Unit containment, negative trusted-kernel tests and deterministic scripted run/inspect/resume E2E coverage.
- [x] T008 Update architecture, roadmap, README/help only where materially required and document all excluded dynamic plugin paths.
- [x] T009 Run focused repetitions, race detector, full repository/CI quality gates and open one PR against `main`.


## Completion

- PR: https://github.com/Anakyklos/Runstead/pull/114 (head `6286f81`, base `main`)
- Full validation: gofmt, `go test ./...`, `go vet ./...`, build, `go test -race ./...` (zero races), `experiments/protocol/test.sh`, `git diff --check`, and all `tools/quality` gates (growth, errcheck, live-convention) green.

## Maintainer review fix (PR #114 review, issue #54)

**Blocker:** `Profile.recipe_ids` was validated but did not bound the effective
recipe surface: `run_recipe` could still execute any recipe of the configured
catalog.

**Fix:** the resolver now materializes one effective `recipe.Catalog`
(`Catalog.Select`) containing exactly the selected recipes (empty
`recipe_ids` + `process.recipes` = whole catalog, deliberate and tested). The
effective catalog is the ONLY surface for the task registry
(`Registry.WithRecipes`), the persisted recipe policy, the frozen contract
(ids + digest over the selection) and every Work Unit view. Non-selected
recipes fail as `unknown_recipe` before any process start.

**Regression tests:**
- `TestResolveRecipeIDsExactAllowlistSurface` (contract/effective/runtime agree; deploy absent)
- `TestResolveEmptyRecipeIDsSelectsWholeCatalogDeliberately`
- `TestResolveRecipeSelectionDriftChangesContract` (selection change = drift; order-neutral)
- `TestResolveRecipeIDSWithoutRunRecipePackageStaysInert`
- `TestRunRecipeEffectiveCatalogSurfaceRejectsUnselected` (execution boundary, no process)
- `TestRunRecipeRestrictedUnitViewKeepsEffectiveSurface` (Work Unit containment)
- `TestProfileRecipeSurfaceExactAllowlistE2E` (real agent loop: deploy rejected, go-test executes, inspect/contract surface)
- `TestProfileRecipeSurfaceApprovalResumeDriftE2E` (policy pause, resume preserves surface/hash, selection drift fails closed)
