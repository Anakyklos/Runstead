# OmniRoute Gateway-Contract Health Design

**Date:** 2026-08-13
**Issue:** #40
**Base:** `origin/main` at `d1f5aef1d1c5ce67f8ba427394e34d8b430f6df8`

## Goal

Add an on-demand, read-only, bounded, fail-closed probe that reports the health of the OmniRoute **gateway management contract** only. It must not claim health for ChatGPT Web, Sentinel, or any private upstream endpoint, and it must not replace authoritative attempt receipts or governor accounting.

## Architecture

Add a provider-neutral typed health value with a conservative zero value:

- `unknown` is the zero value and covers unprobed state, cancellation, timeout, and transport uncertainty.
- `healthy` requires recognized, compatible JSON shapes from all three required management endpoints and unambiguous provider/model evidence.
- `degraded` is reserved for an explicitly recognized temporary management HTTP response. It remains a protected-execution blocker.
- `protocol_changed` covers 404/410, malformed JSON, missing or mistyped fields, ambiguous provider/model evidence, and incompatible shapes.

The provider package exposes a small optional `ContractHealthAware` capability and a sanitized `GatewayContractHealthResult`. The OmniRoute client stores the latest result under its existing mutex, starts at `unknown`, and exposes an explicit `ProbeGatewayContract(context.Context)` method. The probe performs exactly one GET per endpoint, in a fixed bounded sequence, using the existing timeout, response-body limit, redirect rejection, and HTTP seam. It never calls the completion endpoint, sends a POST, retries, follows redirects, uses cookies, rotates accounts, or runs in the background.

The probe reads only:

1. `/api/providers`
2. `/api/settings`
3. `/api/models/alias`

The `/api/settings/model-aliases` and other management evidence used by `Preflight` remain outside this probe. Existing `Preflight` behavior stays unchanged: its management evidence is diagnostic and still fails closed until authoritative receipts exist.

## Execution gates

`RouteSafety` remains the executable declaration for amplification and attempt-accounting behavior. Gateway health is not added to `RouteSafety` and does not mutate it. `Governor.Execute` checks the optional provider-neutral health capability as an additional gate and blocks `unknown`, `degraded`, and `protocol_changed` with a dedicated provider-neutral admission classification. `OmniRoute.Client.Complete` performs the same health check immediately before its completion path so a direct adapter caller cannot bypass the gate. A healthy result never bypasses receipt requirements, durable prepared/finished attempt accounting, policy, circuit, budget, or authoritative upstream attempt accounting.

Providers without the optional capability retain their existing behavior and do not import OmniRoute. The existing RouteSafety and receipt checks remain independent and execute as before.

## Classification and sanitization

The health result carries only a typed state, a fixed reason code, an optional fixed management endpoint identifier, and the existing clock timestamp convention. No raw response body, authorization value, API key, cookie, session/account identifier, prompt, model response, or arbitrary remote error text is retained or emitted. Management HTTP 429 and 5xx statuses that are explicitly recognized as temporary produce `degraded`; 404/410 produce `protocol_changed`; context cancellation/deadline and transport uncertainty produce `unknown`; successful but malformed or incompatible bodies produce `protocol_changed`. Redirects are returned as non-followed responses and never replay a request.

The latest result is exposed through the existing diagnostics/event path using the unambiguous `gateway_contract_health` name. It is not persisted as durable governor/accounting state and is not rendered as `upstream_health`, `provider_health`, or `chatgpt_health`.

## Testing

Reuse the issue #43 contract mock and embedded synthetic corpus. Add focused deterministic coverage for initial `unknown`, all three safe fixtures yielding `healthy`, settings shape drift, ambiguous provider evidence, 404/410, malformed JSON/shape, timeout, cancellation, explicit temporary degradation, no `/v1/chat/completions`, no retry, bounded counts, redirect non-replay, direct and governor fail-closed execution gates, healthy-without-receipts, trace classification, and secret/body redaction. Keep the fixture-hygiene tests green and do not add a second fixture framework.

No live probe, scheduler, polling loop, dependency, account/session rotation, retry, fallback, or browser integration is part of this change.
