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
- Authentication: `reference_required`, `NVIDIA_API_KEY`; loaded from the operator-owned external env file in an isolated process.

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
- Result: `request_count=1`, `error_type=HTTPError`. The executed harness did not emit `exc.code`; the HTTP response status is **unknown/not retained**. The `HTTPError` proves that urllib surfaced an HTTP error response, but its subtype cannot be determined from preserved evidence. No response body or arbitrary headers were retained.
- No fallback, rotation, model sweep, retry, or second control request occurred.

**Stage 1 = NOT PASSED. Failure subtype = INCONCLUSIVE. Reason = experiment-harness observability gap.** The #157 requirement to classify exactly as provider/model protocol, operator/environment, or Runstead defect remains **UNPROVEN**. There is not enough evidence to attribute this failure to the provider/model. There is also no evidence of a Runstead runtime defect: no Runstead task was created, so task-level provider attempts, admissions, debits, actions, tool results, observations, and verifier attempts are all zero.

No replay is authorized solely to obtain better diagnostics. The one controlled-rerun allowance remains unused; replay safety for the direct completion request was not established.

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
- credential value: **NOT tracked**.
- credential derivation/hash: **NOT tracked**.
- operator env-file path: **tracked in the executed harness** at `experiments/provider-live/nvidia-nim-laguna-xs-2-1-canary-v1/stage1_models.py`.
- Secret value emitted in the sanitized execution output: no.
- Raw provider body, private transcript, prompt output, and arbitrary headers retained: no.

## Future harness limitation

Before any future canary, its harness must preserve sufficient sanitized, allowlisted HTTP failure fields to classify the result, including the numeric status. It must continue to omit response bodies, arbitrary headers, and secrets. This PR records the limitation only; it does not implement reusable harness changes.

## Delivery and merge boundary

Exactly one PR is open: [#158](https://github.com/Anakyklos/Runstead/pull/158), titled `test(provider): run NVIDIA NIM Laguna XS 2.1 Gate A canary v1 (#157)`. It targets `main` from this branch. Do not merge it and do not start #123.
