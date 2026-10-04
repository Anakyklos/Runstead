# Groq GPT-OSS-120B Gate A canary v2 — NOT QUALIFIED

Date: 2026-10-04 UTC

Issue: [#141](https://github.com/Anakyklos/Runstead/issues/141)

PR: [#142](https://github.com/Anakyklos/Runstead/pull/142)

PR source branch: `issue-141-groq-oss-canary-v2`
Local worktree branch: `issue-141-groq-gpt-oss-canary-v2`

Base: `8595a660ed4b4be9ff484b1ecfd8c44e5d89de29` (`origin/main` at experiment start)
Stage 2 dispatch HEAD: `3acda03d963a21a4df79467e7f8ff2d55fe82d7d`
Initial report-only PR HEAD: `d9773b839a7827d0434af8b8ec2acb61f7279cef` (before this documentation audit update)

## Decision

**Gate A is NOT QUALIFIED.** Stage 1 passed. The single Stage 2 trajectory ended with Runstead exit 27 and typed outcome `final_not_grounded`; it produced no task tool actions, read observations, or verifier attempt. The trajectory is preserved and was not retried. Stages 3 and 4 were not run. The compatibility document was not changed, #121 was not advanced, and #123 remains blocked by Gate A.

## Candidate and secret handling

| Field | Value |
|---|---|
| Provider ID | `groq-gpt-oss-120b-canary-v2` |
| Protocol family | `openai_compatible` |
| Endpoint | `https://api.groq.com/openai/v1` |
| Model | `openai/gpt-oss-120b` |
| Authentication reference | `GROQ_API_KEY` (external; value omitted) |
| Provider profile version | `v1` |
| Adapter version | `compatible-provider-v0.1` |
| Stage 2 sanitized config identity | `provider.Config{ProviderID:"groq-gpt-oss-120b-canary-v2" ProtocolFamily:"openai_compatible" Endpoint:"https://api.groq.com/openai/v1" Model:"openai/gpt-oss-120b" AuthRequirement:"reference_required" AuthRef:true Options:[] ProfileVersion:"v1" RouteSafety:provider.RouteSafety{AttemptAccounting:0x1, SingleAttempt:0x1, InternalRetries:0x1, CooldownReplay:0x1, AccountPooling:0x1, AutomaticFallback:0x1, ComboRouting:0x1} ConfigVersion:"v1"}` |

The secret was checked for presence and loaded from the ignored local environment file into the request process. Its value was never printed or written to the candidate configuration, task database, evidence, report, or issue. A read-only scan of all retained Stage 2 state files found no literal secret value. Captured CLI output and the unverified final note were not emitted in logs or this report.

## Stage 1 — PASS

- Built the binary from the fresh `main` base and resolved the ref-only candidate through `openai_compatible` before any task dispatch.
- The deterministic missing-acceptance preflight stopped after provider resolution; it created no task state and made no provider request.
- Sent exactly one authenticated `GET https://api.groq.com/openai/v1/models` using an explicit Runstead User-Agent. Result: HTTP 200; exact model `openai/gpt-oss-120b` was present.
- No other model, endpoint, account, quota, or fallback was probed. The response body and credential were not printed or persisted.

## Stage 2 — NOT QUALIFIED

The frozen objective was:

```text
Read app/calc.go and identify the whitespace-handling bug in ParseValues.
Do not modify files and do not run recipes/processes.
Base the final claim on the actual read_file evidence.
```

Before dispatch, the deterministic preflight passed against the fixture’s `app/calc.go` SHA-256 `b8a1bd5dc67bfd9a64bd13503994fdf5e3fc78edaf80502f29992561c6986d94`. It pinned a `file_hash` acceptance check for that file, `repo.read@1.0.0`, and an independent auditor that checks persisted task identity, the exact canonical frozen contract and tool schema, observed file-content hash, citation grounding, final claim, provider accounting, actions, and terminal verifier result. Its claim check accepts one positive sentence naming `ParseValues` and `strconv.Atoi(part)`, identifying the missing trim and whitespace parse failure; the preflight checks that an appended contradictory sentence is rejected.

The process boundary was frozen in issue #141 before dispatch. Only `read_file` and `list_files` could qualify; process-backed `search_text`, `git_status`, and `git_diff`, recipes, and writes would fail the stage. The fixture was copied without Git metadata and the Runstead CLI received an isolated empty `PATH`. Runstead’s optional Git observation was therefore unavailable without starting a subprocess. The CLI invocation itself was the required experiment harness.

| Evidence | Result |
|---|---|
| Task | `cli-1791085826705830653`, one fresh task, `resume_count=0` |
| Terminal | `status=failed`, outcome `final_not_grounded`, exit 27 |
| Provider identity | Exact provider/family/model and sanitized config recorded in the frozen contract and provider attempt |
| Frozen contract | SHA-256 `9b60468c09da8adee94df071f1f0309275071229a05d65c5d6fb49e6f1236cb9`; profile `groq-canary-v2-read-only@1.0.0`; protocol `runstead.protocol.v1` |
| Acceptance plan | Digest `3c1206c11be99bbd41f780830b899902ba3bb17c807d67081a30bfdd9e932af1`; pinned `app/calc.go` hash check |
| Physical provider requests | 1; upstream reached; completed and certain |
| Governor | 1 prepared admission before completion; 1 debit; 0 retries |
| Request and admission identifiers | Execution `exec-000001`; client request `cli-1791085826705830653-0001`; sanitized request ID `sha256:9ef7d39dd4a494d1`; prepared/completed event sequence 4/5; governor ledger row 2 |
| Task actions / tool attempts / tool results | 0 / 0 / 0 |
| Action / observation IDs | None; no task action or observation was persisted |
| Verifier attempts | 0; the task did not reach independently verified completion |
| Verifier / recipe IDs | None; no verifier or recipe was run |
| Process/write effects | No task tool action or attempt; no recipe was exposed; no process-backed observation action was attempted |
| Resume / rerun / interruption | `resume_count=0`; no rerun or interruption/resume; Stage 3/4 were not dispatched |

The provider request completed, but the task finalized as `final_not_grounded` before any actual `read_file(app/calc.go)` observation. Therefore the objective, evidence, acceptance, and terminal-completion requirements were not met. This is classified as a protocol/evidence-grounding failure for this trajectory; it is not evidence that the Stage 2 objective passed. The single trajectory is retained as-is, with no rerun or post-run acceptance change.

## Stages 3–4 — NOT RUN

Stage 3 was gated on Stage 2 PASS. Stage 4 was gated on Stage 3 PASS. Neither stage sent a request or created a task.

## Preflight and validation record

Before Stage 2 dispatch:

- `python3 experiments/provider-live/groq-gpt-oss-120b-canary-v2/preflight_stage2.py` — PASS.
- `python3 experiments/provider-live/groq-gpt-oss-120b-canary-v2/stage2-runner.py --preflight-only` — PASS.
- `python3 -m py_compile .../preflight_stage2.py .../stage2-runner.py` — PASS.
- `git diff --check` — PASS.
- Independent read-only Worker review — approved exactly one dispatch after reviewing contract, evidence, process boundary, accounting, secrecy, and claim checks.

All required pre-PR gates passed on this report-only branch:

- `test -z "$(gofmt -l .)"` — PASS.
- `go test ./...` — PASS; `cmd/runstead` completed in 313.030s.
- `go vet ./...` — PASS.
- `go build -o /tmp/runstead-v2-pr ./cmd/runstead` — PASS.
- `go test -race ./...` — PASS; `cmd/runstead` completed in 406.229s.
- `bash experiments/protocol/test.sh` — PASS.
- `git diff --check` — PASS.

PR #142 was open and unmerged when this report was prepared for review. Its initial report-only head is recorded above; consult current PR metadata for the latest head. This experiment makes no compatibility or Gate A success claim.

## Historical evidence

Issue [#138](https://github.com/Anakyklos/Runstead/issues/138) remains closed `not_planned` and unchanged. Its Stage 2 acceptance checked `README.md` existence rather than the `app/calc.go` objective and remains **NOT QUALIFIED**. PRs [#139](https://github.com/Anakyklos/Runstead/pull/139) and [#140](https://github.com/Anakyklos/Runstead/pull/140) remain unchanged historical evidence. This report is a fresh experiment record under #141 and does not retroactively alter those conclusions.
