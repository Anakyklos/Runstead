# Run Final Output Evidence Design

**Date:** 2026-08-10  
**Issue:** #12 acceptance item: `Final output cites actual evidence identifiers, changed files, diff and process results.`

## Goal

After a deterministic task reaches a durable `completed` state, the normal output of `runstead run` and `runstead resume` must expose a bounded, sanitized, independently verifiable runtime result without requiring a follow-up `runstead inspect`.

The model's final response remains unverified model output. It cannot become the source of the completion report, evidence identifiers, changed-file attribution, Git diff, process results or verifier decision.

## Existing authority

The agent loop already persists the authoritative data needed for the projection:

- the task status, outcome, stop reason and verifier-produced summary;
- successful tool observations in `tool_results`, linked to `tool_attempts` and real evidence IDs;
- structured `WriteEvidence` for writes;
- structured `recipe.Evidence` for every started recipe process, including non-zero exits;
- the task-start Git status/diff baseline and truncation flags;
- the independent verifier report with decision, checks, Git observation, changed-file attribution and bounded current diff;
- durable history that survives `resume` and provider-session replacement.

`runstead inspect` already loads and renders these records. The implementation will reuse its projection loaders instead of adding a second Git, verifier or evidence authority.

## Architecture

### Shared durable projection

Extract the data-loading part of `internal/state/inspect.go` into one private task projection. `RenderInspect` will render the existing historical/detail view from that projection. A new `Store.RenderFinal` will render a smaller final-result view from the same projection.

The projection will add only missing presentation fields:

- the persisted evidence ID on each process result;
- the bounded verifier `CurrentDiff` and its truncation state.

No SQLite rows, raw provider bodies, raw process output or model transcript will be dumped.

### Final renderer contract

`RenderFinal(ctx, out, taskID)` will load the persisted projection and fail closed unless:

- the task status is `completed`;
- the task outcome is `completed`;
- the latest verification attempt exists and has decision `passed`.

For a valid completed task it will render bounded sections for:

1. final state/outcome and verifier decision;
2. verifier summary and acceptance checks;
3. actual persisted evidence IDs, derived from successful tool-attempt/tool-result rows;
4. pre-existing versus during-task changed files from the verifier's Git observation;
5. the real bounded current Git diff, with explicit truncation;
6. every persisted `run_recipe` result with recipe ID, evidence ID, exit code, signal/timeout/cancellation flags and explicit stdout/stderr truncation flags.

The renderer will identify the final result as a verified runtime projection and will not print the model's final summary. The CLI will continue to print model text separately as `note (unverified)`.

### CLI integration

`runstead run` and `runstead resume` will keep their current typed `agent.Result` output for every outcome. Only when the loop returns `OutcomeCompleted` will the CLI invoke `RenderFinal` using the already-open durable store. A failed, blocked, uncertain, approval-paused or incomplete task will not receive a `completed` report.

If the durable final projection cannot be rendered, the CLI will emit a diagnostic and not claim that the final evidence report was shown. The live OmniRoute path remains fail-closed and unchanged.

## Boundedness and sanitization

- Persisted report fields remain subject to the existing verifier bounds and state redaction.
- The final renderer adds a separate small line bound for diff and descriptive fields, with an explicit marker when renderer-level truncation occurs.
- Process stdout/stderr contents remain omitted from the normal final report; only structured process metadata and truncation flags are exposed.
- No credentials, cookies, tokens, private prompts, raw provider bodies or SQLite dump are rendered.

## Testing

- Extend the real CLI coding-loop fixture to assert the normal completed output contains the completed outcome, verifier pass, real evidence IDs, `app/calc.go` as a during-task file, bounded real diff, recipe results and both failed and final zero-exit process evidence.
- Add state renderer tests for process evidence IDs, diff rendering and explicit truncation.
- Add CLI coverage that a model claim such as `tests passed` without recipe evidence remains only an unverified note and does not create a test-success claim in the verified runtime projection.
- Add coverage that final output is absent for a failed or blocked verification result.
- Reuse existing tests for fabricated evidence IDs, pre-existing changes, redaction, fail-closed live provider and resume effect/evidence durability; add only assertions needed for the new normal-output surface.

## Documentation

Update `docs/coding-loop.md`, `docs/verification.md` and the relevant CLI help to state that:

- completed `run`/`resume` output includes enough authoritative evidence to check the task;
- model output and verified runtime result are separate;
- `runstead inspect` remains the historical and detailed view;
- the live ChatGPT Web criterion is still blocked by `#29 -> #30 -> #4`.

## Non-goals

This design does not change the coding loop, verifier rules, approval rules, governor/accounting, attempt receipts, provider activation, process capability policy, generic shell behavior, live providers, or any unrelated issue.
