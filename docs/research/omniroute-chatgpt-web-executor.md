# OmniRoute ChatGPT Web executor — reverse-engineering findings

**Status:** accepted as design input (research only; no Runstead code was changed by the audit)

**Date:** 2026-08-07

**Audited reference:** OmniRoute `release/v3.8.50`
**Fixed SHA:** `976d670ff3a7712df0c695f13095c43eace5e29b` (MIT)
**Full report (pt-BR):** `~/Downloads/omniroute-chatgpt-web-reversa-2026-08-07.md`
**Audit clone:** `~/Downloads/omniroute-audit/omniroute` (isolated, static inspection only)

## Purpose

Record what the OmniRoute `ChatGptWebExecutor` taught us about the ChatGPT Web
internal protocol, session lifecycle, streaming and failure modes, so that the
OmniRoute adapter and the later first-party connector are designed from
verified behavior instead of assumptions. No account, cookie or token was used;
everything below is confirmed by static reading of the pinned SHA.

## Reconstructed flow

```text
cookie (__Secure-next-auth.session-token) → GET /api/auth/session → JWT (cache 5 min)
  → GET / (dpl + script src, cache 1 h)
  → warmup GET /backend-api/{me,conversations,models} (cache 60 s)
  → PATCH user_last_used_model_config (thinking effort, optional)
  → POST /backend-api/sentinel/chat-requirements/prepare   (prekey, PoW "gAAAAAC", target 0fffff)
  → POST /backend-api/sentinel/chat-requirements           (chat-requirements-token)
  → PoW SHA3-512(seed + b64(config)) ≤ difficulty          ("gAAAAAB…")
  → POST /backend-api/f/conversation                       (action:"next", SSE cumulative)
  → deltas via diff + echo suppression
  → handoff: POST /backend-api/f/conversation/resume (x-conduit-token) or poll conversation
  → citation cleanup → OpenAI chat.completion / SSE
```

## Findings that shape Runstead design

1. **Conversation-per-request.** Each turn starts a fresh Temporary Chat
   conversation (`conversation_id: null`, `history_and_training_disabled: true`).
   Prior turns are folded into the system message because sending them as real
   messages makes the model continue the previous answer (observed `[1] →
   [12] → [1123]` across turns). Runstead must not assume conversation
   continuity through OmniRoute.
2. **Cumulative SSE, not deltas.** `content.parts[0]` is the full text so far;
   deltas are computed by slicing from the emitted length, and echoed prior
   turns are suppressed until the current message id reaches
   `status: "in_progress"`. This is the reference pattern for stale-return
   prevention in any stream reconciliation we build.
3. **The cookie is the credential.** `credentials.apiKey` carries the cookie.
   Token cache is keyed by a 64-bit SHA-256 prefix (the previous 32-bit FNV-1a
   could collide and leak one user's token to another), bounded to 200 FIFO
   entries. Cookie rotation from `Set-Cookie` is persisted back via
   `onCredentialsRefreshed`.
4. **No retry inside the executor.** The chatgpt-web executor overrides
   `execute()` entirely; the base executor's 429/WAF retry loop does not apply.
   401/403 clears the token cache. Delivery states (`not_sent` / `sent_confirmed`
   / `sent_unconfirmed` / `response_started` / `completed`) are the right unit
   for Runstead's idempotency work and complement issues #29/#30.
5. **Tool calling is prompt-emulated.** When `tools` are present, a `<tool>`
   contract is injected and parsed back into `tool_calls`; streaming is
   buffered in tool mode. Runstead already refuses this path: model output is
   text interpreted through the Runstead action protocol.
6. **Usage is estimated.** `ceil(len/4)` for prompt and completion tokens.
   Any usage surfaced through OmniRoute must be treated as approximate.
7. **Hard-coded frontend values.** `OAI-Client-Version` / `OAI-Client-Build-Number`
   captured from a real session (April 2026) are brittle; a drift detector is
   required for any adapter that depends on such values.
8. **Encryption at rest exists upstream.** OmniRoute stores fields as
   AES-256-GCM `enc:v1:<iv>:<ct>:<tag>` (see `src/lib/db/encryption.ts`). This
   is the reference for the session vault Runstead plans for Stage 2.

## Practices rejected for Runstead

- TLS impersonation (`tls-client-node`, Firefox 148 profile) and browser
  fingerprint mimicry in the Sentinel prekey (`webdriver−false`, U+2212 keys,
  `_reactListening…`).
- Page-load warmup designed to score a session as "warm" for Sentinel.
- Hard-coded build values without a probe.
- Silent account fallback and cooldown replay.
- Retry after ambiguous delivery without an idempotency key.
- Prompt-emulated tool calling for the critical path.

The OmniRoute adapter remains the baseline; these findings document what a
first-party connector must not reproduce and what the runtime must assume
about the transport.

## Proposed follow-up issues

The audit proposes small, independently shippable work items (created as
GitHub issues referencing this document):

| Area | Issue | Purpose |
|---|---|---|
| Delivery contract | [#38](https://github.com/RenyEnnos/Runstead/issues/38) — delivery states + idempotency key in `provider.Client` | complements #29/#30 |
| Telemetry | [#39](https://github.com/RenyEnnos/Runstead/issues/39) — minimal request telemetry in `ResponseMetadata` | first-token latency, adapter version, session fingerprint |
| Drift detection | [#40](https://github.com/RenyEnnos/Runstead/issues/40) — health probe on the OmniRoute adapter | healthy / degraded / protocol_changed, fail-closed on ambiguity |
| Session vault | [#41](https://github.com/RenyEnnos/Runstead/issues/41) — `sessionvault` package (AES-256-GCM, rotation, redaction) | Stage 2 foundation, no use yet |
| Stream reconciliation | [#42](https://github.com/RenyEnnos/Runstead/issues/42) — `internal/stream` with `FinalTurn` and stale detection | Stage 2 pattern from the cumulative-SSE echo suppression |
| Contract tests | [#43](https://github.com/RenyEnnos/Runstead/issues/43) — redacted fixtures + mock server for the provider boundary | drift guards and parser tests |
| Decision record | this document + architecture section ([PR #37](https://github.com/RenyEnnos/Runstead/pull/37)) | practices rejected and provider boundary |

## Open questions

- Sentinel PoW acceptance of unresolved tokens may change without notice.
- `OAI-Client-Version` (April 2026 capture) still accepted? Not verified.
- Cookie rotation frequency and limits on concurrent turns per account are
  not measured; OmniRoute's rate-limit headers remain the observable signal.
- `conversation/resume` offset semantics (0,1,2) inferred from code, not
  documented upstream.

## Evidence index

Full evidence table (file → function → test) lives in the pt-BR report. Key
files at the pinned SHA: `open-sse/executors/chatgpt-web.ts` (+ `models.ts`,
`citations.ts`, `handoff.ts`), `open-sse/executors/base.ts`,
`open-sse/utils/nextAuthCookie.ts`, `open-sse/services/chatgptTlsClient.ts`,
`src/app/api/v1/{chat/completions,messages,responses}/route.ts`,
`tests/unit/chatgpt-web*.test.ts`.
