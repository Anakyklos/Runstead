# NVIDIA NIM Poolside Laguna XS 2.1 Gate A canary v2 — 2026-10-05

## Trajectory identity

- Issue: [#163](https://github.com/Anakyklos/Runstead/issues/163), child of #121.
- Branch: `issue-163-nvidia-nim-laguna-xs-2-1-canary-v2`.
- Base: `83e628f25c3b1e29dd179be82ce2f6c65c5074d1` (`origin/main`, after sync).
- Provider: `nvidia-nim-laguna-xs-2-1-canary-v2`.
- Protocol family: `openai_compatible` only.
- Model: `poolside/laguna-xs-2.1`.
- Auth reference: `NVIDIA_API_KEY`; credential remained in the operator-owned external env file.
- Stage task IDs: none; Stage 1 failed before task creation.

## Dated official NVIDIA evidence

Checked 2026-10-05, before dispatch:

- NVIDIA's [Poolside Laguna XS 2.1 model page](https://build.nvidia.com/poolside/laguna-xs-2.1) showed the exact model ID, generated OpenAI client example with `base_url = "https://integrate.api.nvidia.com/v1"` and `api_key = "$NVIDIA_API_KEY"`, and `Free Endpoint: Available`.
- NVIDIA's [Poolside Laguna XS 2.1 API reference](https://docs.api.nvidia.com/nim/reference/poolside-laguna-xs-2-1) identified the model and links to its chat-completions API reference.
- This is mutable upstream evidence for this date only. It does not prove the cause of this request's response or any future availability.

## Preflight and Stage 1 — FAIL

- Fresh detached worktree was created from the synced base; it became this issue's sole branch only after preflight passed.
- External env-file checks: `env_file_regular=true`, `nvidia_key_declaration_present=true`, `nvidia_key_loaded=true`, `nvidia_key_nonempty=true`, `secret_value_emitted=false`.
- Local resolver: exact provider ID, `openai_compatible`, NVIDIA base URL, model, and `NVIDIA_API_KEY` reference resolved. `adapter_constructed=false`; `provider_dispatches=0`; `provider_requests=0` before control.
- The harness-specific offline tests passed before dispatch. The direct request uses `sanitized_http.py` and contains only the requested model, user message `Reply with OK.`, `max_tokens=1`, and `stream=false`; no tuning fields were added.
- Provider requests: **1**. Governed Runstead tasks/attempts/admissions/debits: **0 / 0 / 0 / 0**.
- Sanitized HTTP observation:

```text
request_count=1
method=POST
path=/chat/completions
http_status=503
error_type=HTTPError
```

- Separate exact response model/shape validation: `false` (there was no successful completion response to validate).
- Stage 1 is **FAIL** because the control received HTTP 503. The preserved evidence establishes an HTTP response status only; it does not attribute the underlying cause to authentication, model availability, NVIDIA infrastructure, or Runstead.
- No hidden retry, redirect, fallback, rotation, alternate model, account, or provider was used.

## Stages 2–4

- Stage 2 read/evidence/verifier: **SKIPPED**, gated on Stage 1 PASS. No task was created; attempts/admissions/debits, actions, writes, recipes/processes, and verifier attempts are all zero.
- Stage 3 bounded coding: **SKIPPED**, gated on Stage 2 PASS. No workspace effect or test recipe was run for this stage.
- Stage 4 interruption/resume: **SKIPPED**, gated on Stage 3 PASS. No interruption, inspect, or resume was attempted.

## Rerun decision and secret hygiene

- Controlled rerun: **not used**. The exact provider configuration has single-attempt accounting, internal retries disabled, cooldown replay disabled, account pooling disabled, and automatic fallback disabled. Available policy/evidence does not establish a safe replay. The observed 503 is preserved as evidence, not authorization to retry or change identity.
- Secret value, size, hash, prefix, and suffix were never emitted or persisted. The env file was not copied into the repository, task state, logs, prompt, or fixtures. The harness retained no raw response body, arbitrary headers, exception text, raw URL/query, or private transcript.

## Gates and limitations

- Gate A: **NOT SATISFIED**. Stage 1 failed; Stages 2–4 were not run.
- No Runstead task-level governor outcome or verifier result exists because Stage 1 did not pass.
- The single external response is insufficient to identify a root cause or qualify this provider/model for Gate A.
## Local gates and GitHub CI

Initial local verification on the same source contents later committed to the PR:

- `test -z "$(gofmt -l .)"`: PASS.
- `go test ./...`: PASS (`cmd/runstead` 316.401s).
- `go vet ./...`: PASS.
- `go build ./cmd/runstead`: PASS.
- `go test -race ./...`: PASS (`cmd/runstead` 497.960s).
- `bash experiments/protocol/test.sh`: PASS.
- `python3 -m unittest -v test_sanitized_http.py`: PASS (4 tests).
- NVIDIA v2 harness tests: PASS (7 tests).
- `git diff --check`: PASS.

GitHub CI: [run 37387767805](https://github.com/Anakyklos/Runstead/actions/runs/37387767805) completed successfully on HEAD `573dcebb116570e393c62002b66d078f3456ffc1`. The PR checks page is https://github.com/Anakyklos/Runstead/pull/164/checks. This records the CI evidence for the reviewed canary commit; the following traceability-only commit changes no harness or canary result.
