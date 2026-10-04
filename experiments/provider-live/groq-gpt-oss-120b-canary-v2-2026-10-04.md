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

The original dispatch record states that the secret was checked and loaded from the ignored local environment file into the request process, and that captured CLI output and the unverified final note were withheld. Those historical handling claims were not independently reproduced during this review: `GROQ_API_KEY` was unset, so the post-review bounded scan reports `secret_scan=unavailable` and makes no claim that the retained state is free of the literal secret. The earlier sentence claiming that a read-only scan found no secret is superseded and remains unverified here. No credential value was read, printed, or added to this report during this review.

## Stage 1 — PASS

- Built the binary from the fresh `main` base and resolved the ref-only candidate through `openai_compatible` before any task dispatch.
- The deterministic missing-acceptance preflight stopped after provider resolution; it created no task state and made no provider request.
- Sent exactly one authenticated `GET https://api.groq.com/openai/v1/models` using an explicit Runstead User-Agent. Result: HTTP 200; exact model `openai/gpt-oss-120b` was present.
- No other model, endpoint, account, quota, or fallback was probed. The response body and credential were not printed or persisted.
- The sanitized Stage 1 record is this report; no request-correlation ID or separate HTTP trace was retained. This direct preflight request did not create a Runstead task admission/debit row.

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

### Post-review read-only evidence audit

The P1 correction added an opt-in `--audit-existing` path. It opens the existing SQLite database with SQLite read-only/query-only settings and never invokes the Runstead executable or a provider. The audit command was:

```text
python3 experiments/provider-live/groq-gpt-oss-120b-canary-v2/stage2-runner.py --audit-existing --state-dir /tmp/groq-gpt-oss-v2-stage2-state --task-id cli-1791085826705830653
```

Result: `db_predicates=pass`, `secret_scan=unavailable`, `audit_result=LIMITED`, exit code 3, and `secret_absence=not_proven`. The database recheck reproduced the task ID/status/outcome and `resume_count=0`; one completed, certain, upstream-reached provider attempt with the expected provider/family/model/config identity and reconciled execution/client/upstream request IDs; one admission and debit with zero retries; the frozen acceptance digest saved before dispatch; and zero task actions, tool attempts/results, verifier attempts, write-policy decisions, and approvals. The exact canonical frozen contract and acceptance plan also matched the pinned experiment inputs.

The SQLite record does not persist the original CLI exit status, so exit 27 is part of the original harness record and was not reproduced by this audit. The audit confirms one persisted upstream-reached attempt; the SQLite ledger alone does not independently establish physical HTTP request count. Provider/model behavior, secret absence, and the historical Stage 1 secret-handling claim were not revalidated. This evidence checker is experiment tooling only; it is not Runstead's verifier, policy, governor, or source of truth. Its result does not qualify Stage 2 or Gate A.

## Stages 3–4 — NOT RUN

Stage 3 was gated on Stage 2 PASS. Stage 4 was gated on Stage 3 PASS. Neither stage sent a request or created a task.

The Stage 2 SQLite ledger is scoped to its single task; it is not a global HTTP request ledger. The Stage 3/4 stop is recorded by the orchestration decision in this report, issue #141, and PR #142. No Stage 3/4 task, recipe, verifier, interruption, resume, or replay identifiers exist.

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

For the P1 audit correction, 17 offline synthetic SQLite tests passed, including the expected failure shape, attempt and ID mismatches, late acceptance events, unexpected action/verifier rows, retry/debit mismatches, secret presence, symlink/incomplete and bounded-size scans, unavailable-key limitation, sanitized preflight failure, and checks that the database is not modified and the Runstead executable is not invoked. `py_compile` passed for both Stage 2 Python files. These are audit-tool checks; they do not qualify the canary.

PR #142 was open and unmerged when this report was prepared for review. Its initial report-only head is recorded above; consult current PR metadata for the latest head. This experiment makes no compatibility or Gate A success claim.

## Historical evidence

Issue [#138](https://github.com/Anakyklos/Runstead/issues/138) remains closed `not_planned` and unchanged. Its Stage 2 acceptance checked `README.md` existence rather than the `app/calc.go` objective and remains **NOT QUALIFIED**. PRs [#139](https://github.com/Anakyklos/Runstead/pull/139) and [#140](https://github.com/Anakyklos/Runstead/pull/140) remain unchanged historical evidence. This report is a fresh experiment record under #141 and does not retroactively alter those conclusions.
