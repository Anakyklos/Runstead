# NVIDIA NIM Laguna XS 2.1 Gate A canary v1 — 2026-10-05

## Identity and starting state

- Parent issue: [#121](https://github.com/Anakyklos/Runstead/issues/121)
- Canary issue: [#157](https://github.com/Anakyklos/Runstead/issues/157), linked as its native sub-issue
- Branch: `issue-157-nvidia-laguna-xs-2-1-canary-v1`
- Base / starting HEAD: `origin/main` / `42a1d8f9c221571cdf53038faef1e5c18d34935a`
- Provider: `nvidia-nim-laguna-xs-2-1-canary-v1`
- Protocol: `openai_compatible`
- Base URL: `https://integrate.api.nvidia.com/v1`
- Model: `poolside/laguna-xs-2.1`
- Authentication: `reference_required`, `NVIDIA_API_KEY`; loaded from the operator-owned external env file in an isolated process. The file path and credential value are not in tracked artifacts.

The previous checkpoint was a **preflight environment propagation failure before trajectory creation**, not an NVIDIA canary failure. It had zero provider requests, tasks, attempts, admissions, and debits and consumed no controlled rerun. It created no issue or branch. This v1 continues that preparation; it does not create a second trajectory.

## Dated external evidence

Checked 2026-10-05:

- NVIDIA's [Laguna XS 2.1 model page](https://build.nvidia.com/poolside/laguna-xs-2.1) documents the exact model ID, OpenAI client base URL, and chat-completions usage.
- NVIDIA's [LLM API reference](https://docs.api.nvidia.com/nim/reference/llm-apis) documents the hosted LLM API base and `POST /v1/chat/completions`.

These are mutable upstream references. They do not establish account entitlement, current availability, or the outcome of this canary request.

## Stage 1 — auth/model: NOT PASSED

### Pre-dispatch acceptance and evidence

The objective, acceptance checks, allowed effects, prohibited effects, and stop conditions are frozen in `stage1-preflight.json`.

- Exact declaration resolved through Runstead's existing provider registry: PASS.
- Resolution output matched the fixed provider ID, family, base URL, model, auth requirement, and auth reference.
- `adapter_constructed=false`; pre-control `provider_dispatches=0`.
- Explicit-path isolated loader booleans all passed:
  - `env_file_path_matches_expected=true`
  - `env_file_regular=true`
  - `nvidia_key_declaration_present=true`
  - `nvidia_key_loaded=true`
  - `nvidia_key_nonempty=true`
  - `secret_value_emitted=false`
- One authenticated exact-model control request was authorized: `POST /v1/chat/completions`, fixed model only.
- Result: `request_count=1`, `error_type=HTTPError`. The script did not emit the numeric HTTP status, and no response body or arbitrary headers were retained. Exact-model response validity is therefore unknown.
- No fallback, rotation, model sweep, or second control request occurred.

Classification: **provider/model control failure**, with root cause undetermined. The available sanitized result does not establish whether this was authentication, request compatibility, availability, rate/capacity, or another HTTP response. No Runstead task was created, so task-level provider attempts, admissions, debits, actions, tool results, observations, and verifier attempts are all zero. No Runstead runtime defect is established by this stage.

A second control request is not admissible: replay safety for the direct completion request was not established. The one controlled-rerun allowance remains unused.

## Stages 2–4 and Gate A

- Stage 2: SKIPPED; conditional on Stage 1 passing.
- Stage 3: SKIPPED; conditional on Stage 2 passing.
- Stage 4: SKIPPED; conditional on Stage 3 passing.
- Gate A: **NOT SATISFIED**.

The full Gate A contract remains unproven. No downstream work on #123 was started.

## Request and credential accounting

- Provider requests in this v1 trajectory: 1 Stage 1 control request.
- Runstead tasks: 0.
- Governed attempts / admissions / debits: 0 / 0 / 0.
- Automatic retries / fallback / rotation: 0 / 0 / 0.
- Controlled rerun: not used.
- Secret value emitted, logged, committed, persisted, or derived: no.
- Raw provider body, private transcript, prompt output, and arbitrary headers retained: no.

## Delivery and merge boundary

Open exactly one PR from this branch using the issue's specified title. Do not merge it and do not start #123.
