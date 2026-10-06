# NVIDIA NIM Nemotron 3 Super Gate A canary v1

**Current decision: Gate A = NOT SATISFIED. Stage 1 dispatch is pending; Stages 2–4 have not started.** This report will be updated with only the evidence from the authorized one-shot trajectory.

## Identity and source

- Parent: [#121](https://github.com/Anakyklos/Runstead/issues/121)
- Issue: [#168](https://github.com/Anakyklos/Runstead/issues/168)
- PR: [#169](https://github.com/Anakyklos/Runstead/pull/169)
- Preflight source: `c079f80321fde3a687416afb5a055cece9ed4762` (merged PR #167)
- Reviewed PR head before this update: `70218f68491af2318b5034e6a37c38ea04e38615`
- Candidate: `nvidia-nim-nemotron-3-super-120b-a12b-canary-v1`
- Protocol: `openai_compatible`
- Base URL: `https://integrate.api.nvidia.com/v1`
- Model: `nvidia/nemotron-3-super-120b-a12b`
- Auth reference: `NVIDIA_API_KEY`

## Stage 1 control

Stage 1 is a direct sanitized pre-task HTTP control, following the established canary pattern. It sends at most one fixed `POST /chat/completions` request with `max_tokens=1`, rejects redirects, performs no retry or fallback, and reports the HTTP observation plus separate response-shape and exact-model checks. It does not count as a Runstead task, admission, debit, or adapter proof.

Immediately before dispatch, the runbook requires rechecking current NVIDIA documentation, the external env file and key-presence booleans, exact provider resolution with `SafeRouteSafety`, and zero prior provider requests. Dispatch remains pending at this report revision.

## Stage verdicts

| Stage | Verdict | Evidence |
|---|---|---|
| 1 — auth/model | **PENDING** | No control request has yet been sent in this report revision. |
| 2 — read-only protocol/evidence/verifier | **NOT STARTED** | Runs only after Stage 1 PASS. |
| 3 — bounded coding | **NOT STARTED** | Runs only after Stage 2 PASS. |
| 4 — interruption/resume | **NOT STARTED** | Runs only after Stage 3 PASS. |

The absence of a `max_tokens` field in the runtime request contract is not a Gate A blocker: Stage 1 is a direct pre-task control, while Stages 2–4 exercise the real adapter, governor, and runtime.

## Verification in this revision

- Targeted offline canary tests: pending final local gates.
- Live NVIDIA requests: `0` as of this report revision.
- Runstead tasks: `0` as of this report revision.
- Controlled rerun: not used.
