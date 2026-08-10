# Authoritative Final Run Output Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make completed `runstead run` and `runstead resume` print a bounded verified-runtime projection containing authoritative evidence IDs, changed-file attribution, Git diff and recipe process results, without trusting model output.

**Architecture:** Extract the existing `runstead inspect` data loading into a shared private state projection. Keep `RenderInspect` as the historical detailed renderer and add `Store.RenderFinal` as a smaller completion-only renderer using the same projection and persisted verifier report. The CLI invokes the final renderer only after the loop returns `OutcomeCompleted`, and the renderer independently checks persisted status/outcome/latest verifier decision before emitting the verified section.

**Tech Stack:** Go, `modernc.org/sqlite`, existing `internal/state` persistence, existing verifier report JSON, existing bounded Git/process evidence, Go table/unit/E2E tests.

## Global Constraints

- Use only authoritative durable state, tool/process evidence, real Git observation and the independent verifier report.
- Keep model final text labeled `note (unverified)` and never use it as proof.
- Preserve bounded output, existing redaction, fail-closed states, approval rules, verifier behavior, governor/accounting and attempt-receipt contracts.
- Do not add a second Git attribution mechanism, generic shell, live provider path, credentials, raw provider bodies, raw process output or SQLite dump.
- The final PR must be based on `origin/main` and contain only the #12 final-output acceptance item plus its focused tests/docs.

---

### Task 1: Add failing state-renderer tests for the shared final projection

**Files:**
- Modify: `internal/state/process_test.go`
- Modify: `internal/state/verification_test.go`
- Test target: `internal/state`

**Interfaces:**
- Consumes: existing `openTestStore`, `mustTask`, `RecordAction`, `PrepareToolAttempt`, `CompleteToolAttempt`, `SaveVerificationAttempt`, `RenderInspect` helpers.
- Produces: failing tests that require process evidence IDs, explicit process truncation, a bounded Git diff in inspect, and a new `RenderFinal` method that refuses non-completed/non-passed state.

- [ ] **Step 1: Write the failing process/diff renderer test**

Add a test in `internal/state/process_test.go` that persists a `run_recipe` observation with `EvidenceID: "obs-000001"`, `recipe_id: "test"`, `exit_code: 0`, and both truncation flags false. Persist a passed verification whose report JSON contains an available Git observation with `current_diff: "diff --git a/app/calc.go b/app/calc.go\n+fixed\n"`, `truncated: false`, and during-task `app/calc.go`. Assert `RenderInspect` contains:

```go
if !strings.Contains(rendered, "recipe=test evidence=obs-000001 exit=0") {
    t.Fatalf("inspect must show the process evidence id:\n%s", rendered)
}
if !strings.Contains(rendered, "truncated=stdout:false/stderr:false") {
    t.Fatalf("inspect must show explicit process truncation state:\n%s", rendered)
}
if !strings.Contains(rendered, "Git diff (bounded):") || !strings.Contains(rendered, "+fixed") {
    t.Fatalf("inspect must show the persisted bounded diff:\n%s", rendered)
}
```

- [ ] **Step 2: Write the failing final-renderer state test**

Add a test in `internal/state/verification_test.go` that builds a completed task with one passed verification and calls the new method:

```go
var out strings.Builder
if err := store.RenderFinal(context.Background(), &out, "task-1"); err != nil {
    t.Fatalf("RenderFinal() error = %v", err)
}
for _, want := range []string{
    "Verified runtime result:",
    "status: completed",
    "outcome: completed",
    "verifier: passed",
} {
    if !strings.Contains(out.String(), want) {
        t.Fatalf("final output missing %q:\n%s", want, out.String())
    }
}
```

Also add a table test that calls `RenderFinal` for `status=running` with a blocked/latest verification and for a failed task, expecting a non-nil error and no `Verified runtime result:` text in the writer.

- [ ] **Step 3: Run the focused tests and verify they fail for the missing behavior**

Run:

```bash
go test ./internal/state -run 'Test(RenderInspect|RenderFinal|.*Process.*Evidence|.*Diff)' -count=1
```

Expected: FAIL because process evidence IDs/diff are not rendered and `Store.RenderFinal` does not yet exist. Fix only test setup mistakes if the failure is a compile/test error unrelated to the missing behavior.

- [ ] **Step 4: Commit the red tests**

```bash
git add internal/state/process_test.go internal/state/verification_test.go
git commit -m "test(state): require authoritative final output projection"
```

---

### Task 2: Extract the shared inspect projection and implement `RenderFinal`

**Files:**
- Modify: `internal/state/inspect.go`
- Modify: `internal/state/store.go` only if the renderer interface needs the new method
- Test: `internal/state/process_test.go`, `internal/state/verification_test.go`

**Interfaces:**
- Consumes: existing `inspectTask`, tool/provider/process/verification loaders, `renderGitObservation`, `renderChangedFiles`, `boundedRender`, state redaction.
- Produces: `(*Store).RenderFinal(ctx context.Context, out io.Writer, taskID string) error`, plus `RenderInspect` backed by the same private projection.

- [ ] **Step 1: Define the projection and loader**

Create a private projection type containing the existing inspect values:

```go
type inspectProjection struct {
    task                 inspectTask
    events               []eventRow
    actions              []inspectAction
    toolAttempts         []inspectToolAttempt
    providerAttempts     []inspectProviderAttempt
    receipts             []inspectReceipt
    writeDecisions       []inspectWritePolicyDecision
    approvals            []inspectApproval
    pending              []PendingApproval
    processEvidence      []inspectProcessEvidence
    verificationAttempts []VerificationAttemptRow
    governorState        *inspectGovernor
}

func (s *Store) loadInspectProjection(ctx context.Context, taskID string) (inspectProjection, error) {
    task, err := s.loadInspectTask(ctx, taskID)
    if err != nil { return inspectProjection{}, err }
    events, err := s.loadEvents(ctx, taskID)
    if err != nil { return inspectProjection{}, err }
    actions, err := s.loadInspectActions(ctx, taskID)
    if err != nil { return inspectProjection{}, err }
    toolAttempts, err := s.loadInspectToolAttempts(ctx, taskID)
    if err != nil { return inspectProjection{}, err }
    providerAttempts, err := s.loadInspectProviderAttempts(ctx, taskID)
    if err != nil { return inspectProjection{}, err }
    receipts, err := s.loadInspectReceipts(ctx, taskID)
    if err != nil { return inspectProjection{}, err }
    writeDecisions, err := s.loadInspectWritePolicyDecisions(ctx, taskID)
    if err != nil { return inspectProjection{}, err }
    approvals, err := s.loadInspectApprovals(ctx, taskID)
    if err != nil { return inspectProjection{}, err }
    pending, err := s.PendingApprovals(ctx, taskID)
    if err != nil { return inspectProjection{}, err }
    processEvidence, err := s.loadInspectProcessEvidence(ctx, taskID)
    if err != nil { return inspectProjection{}, err }
    verificationAttempts, err := s.VerificationAttempts(ctx, taskID)
    if err != nil { return inspectProjection{}, err }
    governorState, err := s.loadInspectGovernor(ctx, taskID)
    if err != nil { return inspectProjection{}, err }
    return inspectProjection{task: task, events: events, actions: actions,
        toolAttempts: toolAttempts, providerAttempts: providerAttempts,
        receipts: receipts, writeDecisions: writeDecisions, approvals: approvals,
        pending: pending, processEvidence: processEvidence,
        verificationAttempts: verificationAttempts, governorState: governorState}, nil
}
```

Move the current loader calls from `RenderInspect` into `loadInspectProjection`, then make `RenderInspect` render fields from the returned projection without changing its existing section order or sanitized content.

- [ ] **Step 2: Preserve process evidence identity and explicit truncation**

Change `inspectProcessEvidence` to include `EvidenceID string`. Change its query to select `r.evidence_id`, scan it, and keep the existing bounded JSON field extraction. Render every process line with all status metadata explicitly:

```go
fmt.Fprintf(&builder, "  %s execution=%s recipe=%s evidence=%s exit=%d truncated=stdout:%t/stderr:%t", ...)
```

Append `signal=`, `timed_out=yes`, `canceled=yes`, `duration=`, and `network_isolation=` only when applicable, preserving existing sanitized behavior.

- [ ] **Step 3: Render the persisted bounded Git diff**

Extend the JSON projection in `renderGitObservation` with `CurrentDiff` and `Truncated`. Keep the existing pre-existing/during-task attribution from the verifier. Add a bounded one-line diff representation using the persisted report value, not a new Git command:

```go
if git.CurrentDiff == "" {
    builder.WriteString("  Git diff (bounded): (none)\n")
} else {
    diff := boundedRender(singleLine(git.CurrentDiff))
    fmt.Fprintf(builder, "  Git diff (bounded): %s\n", diff)
}
fmt.Fprintf(builder, "  diff truncated: %t\n", git.Truncated || len(singleLine(git.CurrentDiff)) > maxInspectLine)
```

Keep the existing `baseline_truncated` limitation. Do not claim that pre-existing changes belong to the task.

- [ ] **Step 4: Implement the completion-only final renderer**

Add a private validator and `RenderFinal`:

```go
func (s *Store) RenderFinal(ctx context.Context, out io.Writer, taskID string) error {
    projection, err := s.loadInspectProjection(ctx, taskID)
    if err != nil { return err }
    if projection.task.Status != "completed" || projection.task.Outcome != "completed" {
        return fmt.Errorf("%w: task status=%s outcome=%s", ErrFinalProjectionUnavailable,
            projection.task.Status, projection.task.Outcome)
    }
    if len(projection.verificationAttempts) == 0 {
        return fmt.Errorf("%w: no verification attempt", ErrFinalProjectionUnavailable)
    }
    latest := projection.verificationAttempts[len(projection.verificationAttempts)-1]
    if latest.Decision != "passed" {
        return fmt.Errorf("%w: latest verification decision=%s", ErrFinalProjectionUnavailable, latest.Decision)
    }

    var builder strings.Builder
    builder.WriteString("Verified runtime result:\n")
    fmt.Fprintf(&builder, "  status: %s\n  outcome: %s\n", projection.task.Status, projection.task.Outcome)
    fmt.Fprintf(&builder, "  verifier: %s\n  summary: %s\n", latest.Decision, boundedRender(latest.Summary))
    builder.WriteString("  acceptance checks:\n")
    for _, check := range latest.Checks {
        fmt.Fprintf(&builder, "    check=%s type=%s status=%s\n", check.CheckID, check.Type, check.Status)
        if len(check.Evidence) > 0 { fmt.Fprintf(&builder, "      evidence=%s\n", strings.Join(check.Evidence, ",")) }
    }
    builder.WriteString("  evidence IDs:\n")
    for _, attempt := range projection.toolAttempts {
        if attempt.EvidenceID != "" { fmt.Fprintf(&builder, "    %s tool=%s\n", attempt.EvidenceID, attempt.Tool) }
    }
    builder.WriteString("  Git observation:\n")
    renderGitObservation(&builder, projection.verificationAttempts)
    builder.WriteString("  process results:\n")
    renderProcessEvidence(&builder, projection.processEvidence)
    if _, err := io.WriteString(out, builder.String()); err != nil { return fmt.Errorf("write final output: %w", err) }
    return nil
}
```

The final sections must include `Verified runtime result:`, task status/outcome, latest verifier decision/summary/checks, actual non-empty tool-attempt evidence IDs, Git attribution/diff, and all process results. Do not render `task.Summary` as model output; it is already the verifier-produced persisted summary.

- [ ] **Step 5: Run the focused state tests and verify green**

Run:

```bash
gofmt -w internal/state/inspect.go internal/state/store.go internal/state/process_test.go internal/state/verification_test.go
go test ./internal/state -run 'Test(RenderInspect|RenderFinal|.*Process.*Evidence|.*Diff)' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit the shared projection**

```bash
git add internal/state/inspect.go internal/state/store.go internal/state/process_test.go internal/state/verification_test.go
git commit -m "feat(state): render bounded authoritative final result"
```

---

### Task 3: Add failing CLI tests for normal run/resume output

**Files:**
- Modify: `cmd/runstead/coding_loop_test.go`
- Modify: `cmd/runstead/main_test.go` only for focused non-completed output coverage

**Interfaces:**
- Consumes: existing real fixture helpers, `run`, `inspectRendered`, `taskIDFromOutput`, existing acceptance plans.
- Produces: failing assertions that completed normal output contains the authoritative projection and non-completed output does not contain a verified final report.

- [ ] **Step 1: Extend the real coding-loop E2E assertions**

In `TestCodingLoopDeterministicScenarioEndToEnd`, assert against `out.String()` rather than only `inspectRendered(...)`:

```go
for _, want := range []string{
    "Verified runtime result:",
    "status: completed",
    "outcome: completed",
    "verifier: passed",
    "during-task changes: app/calc.go",
    "Git diff (bounded):",
    "recipe=test evidence=obs-000002 exit=1",
    "recipe=test evidence=obs-000005 exit=1",
    "recipe=test evidence=obs-000008 exit=0",
    "truncated=stdout:false/stderr:false",
} {
    if !strings.Contains(out.String(), want) {
        t.Fatalf("normal run output missing %q:\n%s", want, out.String())
    }
}
```

Also assert that the model text remains separate:

```go
if !strings.Contains(out.String(), "note (unverified): Fixed the calculator.") {
    t.Fatalf("model text must remain explicitly unverified:\n%s", out.String())
}
```

- [ ] **Step 2: Add the no-recipe-claim negative assertion**

Extend the existing CLI scenario whose model final says `tests passed` while the acceptance plan proves only an artifact. Assert the output contains the unverified note and verified artifact check, but does not contain a recipe process result or a verified test-success statement:

```go
if strings.Contains(out.String(), "recipe=test exit=0") || strings.Contains(out.String(), "verified: tests passed") {
    t.Fatalf("model text must not create verified recipe success:\n%s", out.String())
}
```

- [ ] **Step 3: Add a non-completed final-output assertion**

In an existing failed/blocked CLI test, assert `Verified runtime result:` is absent from normal stdout while the typed outcome remains present. Do not add a second verifier invariant test if an existing case already exercises the same state.

- [ ] **Step 4: Run the focused CLI tests and verify they fail**

Run:

```bash
go test ./cmd/runstead -run 'TestCodingLoopDeterministicScenarioEndToEnd|Test.*Final.*Output|TestLoopVerificationModelTextNeverVerifiedSummary' -count=1
```

Expected: FAIL because `run` and `resume` do not yet call the final renderer.

- [ ] **Step 5: Commit the red CLI tests**

```bash
git add cmd/runstead/coding_loop_test.go cmd/runstead/main_test.go
 git commit -m "test(cli): require authoritative final run output"
```

---

### Task 4: Integrate `RenderFinal` into `run` and `resume`

**Files:**
- Modify: `cmd/runstead/main.go`
- Modify: `cmd/runstead/resume.go`
- Test: `cmd/runstead/coding_loop_test.go`, `cmd/runstead/main_test.go`

**Interfaces:**
- Consumes: `state.Store.RenderFinal`, `agent.Result`, existing open store lifecycle.
- Produces: normal completed output for both fresh runs and resumed runs; typed output unchanged for non-completed states.

- [ ] **Step 1: Add one CLI helper for the shared final-output call**

Add a helper near `printResult`:

```go
func printFinalRuntimeResult(ctx context.Context, out, errOut io.Writer, store *state.Store, taskID string, result agent.Result, command string) error {
    printResult(out, errOut, taskID, result)
    if result.Outcome != agent.OutcomeCompleted {
        return nil
    }
    if err := store.RenderFinal(ctx, out, taskID); err != nil {
        fmt.Fprintf(errOut, "%s: verified final output unavailable: %v\n", command, err)
        return err
    }
    return nil
}
```

The helper must never construct evidence IDs or derive fields from `result.Note`, `result.Summary` or model text.

- [ ] **Step 2: Use the helper in `runCommand`**

Replace the direct `printResult` call after `loop.Run` with the helper. Return `exitUnavailable` if rendering the required completed projection fails; otherwise return the existing typed outcome exit code.

- [ ] **Step 3: Use the helper in `resumeCommand`**

Make the same change after the resumed loop returns. Keep all recovery/restart logic untouched. The same durable task ID and store projection must be used, so evidence IDs and prior process results are not replayed or regenerated.

- [ ] **Step 4: Run focused CLI tests and verify green**

Run repeatedly to detect flakiness:

```bash
for i in 1 2 3 4 5; do
  go test ./cmd/runstead -run 'TestCodingLoopDeterministicScenarioEndToEnd|TestCodingLoop.*Resume|Test.*Final.*Output|TestLoopVerificationModelTextNeverVerifiedSummary' -count=1 || exit $i
done
```

Expected: PASS on all five iterations. The deterministic E2E must show process trajectory `exit=1`, `exit=1`, `exit=0` and the final verifier `passed` projection in normal output.

- [ ] **Step 5: Commit CLI integration**

```bash
git add cmd/runstead/main.go cmd/runstead/resume.go
 git commit -m "feat(cli): expose verified runtime result after completion"
```

---

### Task 5: Update focused documentation and help

**Files:**
- Modify: `docs/coding-loop.md`
- Modify: `docs/verification.md`
- Modify: `cmd/runstead/main.go`
- Modify: `cmd/runstead/resume.go`

**Interfaces:**
- Consumes: final renderer output labels and current live-provider blocker text.
- Produces: concise user-facing documentation of `run`/`resume` final output versus `inspect` history.

- [ ] **Step 1: Document the normal completed output**

Add a section to `docs/coding-loop.md` explaining that a completed `run` or `resume` now prints a verified runtime projection with outcome, verifier decision/checks, real evidence IDs, Git attribution/diff and recipe results. State that `inspect` remains the detailed historical view.

- [ ] **Step 2: Clarify model output versus runtime result**

Update `docs/verification.md` to state that `note (unverified)` is model output, while `Verified runtime result:` is loaded from durable state and the independent verifier report. Explicitly state that model claims cannot create recipe evidence, changed-file attribution or success.

- [ ] **Step 3: Update CLI help without changing unrelated contracts**

Update `printRunHelp` and `printResumeHelp` to mention the completion projection and that `runstead inspect <task-id>` remains the detailed historical view. Keep the existing live OmniRoute blocker text unchanged.

- [ ] **Step 4: Run documentation/help tests**

Run:

```bash
gofmt -w cmd/runstead/main.go cmd/runstead/resume.go
go test ./cmd/runstead -run 'TestRootHelp|TestCommandHelp|TestResumeHelpDescribesRecovery|TestCodingLoopDeterministicScenarioEndToEnd' -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit documentation**

```bash
git add docs/coding-loop.md docs/verification.md cmd/runstead/main.go cmd/runstead/resume.go
git commit -m "docs: describe verified run completion output"
```

---

### Task 6: Full verification, review and focused PR

**Files:**
- Verify all changed files from `git diff origin/main...HEAD`
- Test: entire repository and required protocol experiment

- [ ] **Step 1: Inspect scope and formatting**

Run:

```bash
git diff --stat origin/main...HEAD
git diff --check origin/main...HEAD
gofmt -l $(git diff --name-only origin/main...HEAD -- '*.go')
```

Expected: only the focused state/CLI tests, renderer/CLI implementation, documentation and the spec/plan are changed; `gofmt -l` prints nothing.

- [ ] **Step 2: Run the required verification commands**

Run each command fresh and record its exit status/output:

```bash
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/runstead
bash experiments/protocol/test.sh
```

- [ ] **Step 3: Repeat focused tests**

Run the deterministic final-output and resume tests at least five times:

```bash
for i in 1 2 3 4 5; do
  go test ./cmd/runstead -run 'TestCodingLoopDeterministicScenarioEndToEnd|TestCodingLoop.*Resume|Test.*Final.*Output' -count=1 || exit $i
done
```

- [ ] **Step 4: Request focused code review**

Review the diff against `origin/main`, specifically checking that the model never supplies final-output fields, non-completed states never get a verified report, process evidence IDs come from persisted rows, Git attribution is reused from the verifier report, and no #13/#29/#30/#4 behavior changed.

- [ ] **Step 5: Commit any review fixes and verify again**

For every review fix, run the relevant focused test first, then rerun all required commands before claiming readiness.

- [ ] **Step 6: Push and open the focused PR**

After fresh verification, push only `feat/issue-12-final-output-evidence` and open a PR against `main` with:

```text
Refs #12

Implements the internal #12 acceptance item: final output cites actual evidence identifiers, changed files, bounded Git diff and real process results. The live ChatGPT Web criterion remains blocked by #29 -> #30 -> #4.
```

Do not state that all of #12 is complete while the live criterion remains blocked.
