# Bounded governor-owned retry and cooldown (issue #92)

Retry and cooldown for compatible providers are owned by the **governor
frontier**, never by the adapters. Retry scheduling lives in
`agent.Executor`, ABOVE `Governor.Execute`; the governor remains the only
authority for admission, budgets, circuits, cooldown and retry eligibility.

## Authority boundary

```text
agent loop
   |
agent.Executor            <- bounded retry orchestration (#92)
   |
Governor.Execute          <- ADMIT + persist prepared + provider.Client.Complete
   |                          + persist finished + classify eligibility
   v
if retry permitted (governor RetryEligible):
    bounded wait (Retry-After / profile cooldown / circuit cooldown)
    NEW Governor.Execute
    NEW admission / accounting / evidence / physical attempt
```

One `provider.Client.Complete` call remains at most one physical upstream
request. The OpenAI-, Anthropic- and Google-compatible adapters are
unchanged and never retry on their own.

## Retry gate (central rule)

```text
retryable(error)          = governor outcome class in recoverable set AND
                            delivery state proves it was safe
AND governor_allows()     = FinishResult.RetryEligible (retry budget +
                            circuit closed)
AND attempt_budget_remaining()
AND elapsed_budget_remaining()   (context deadline/time budget)
AND circuit_allows()
AND delivery_state_is_safe_for_retry()
```

Retryable classes (explicitly classified, typed, provider-neutral):
`rate_or_capacity` (429), `upstream_server_failure` (transient 5xx),
`empty_response`, `malformed_upstream`, `timeout` — each ONLY when
delivery state proves the effect was not possibly sent (`completed`/
`sent_confirmed`); any `sent_unconfirmed` / `response_started` state
downgrades to uncertain and is never retried.

`connection_reset` remains a governor-level class that OTHER classification
paths may produce; the compatible-provider classifier deliberately does NOT
map it. The compatible adapters expose a generic `transport` kind for
connection/setup failures, and delivery observation cannot prove the request
was never dispatched, so `compat.classifyError` maps `transport` (and every
unknown kind) to `uncertain_reached`: never retried, by construction.

Never retried: auth/permission failures, config/capability mismatches,
refusal, unknown classification, unsafe redirects, request/response too
large (requires reconstruction), timeout/cancel/disconnect after possible
dispatch, and any uncertain delivery. Malformed `runstead.protocol.v1`
produced by the model is a NEW model turn (protocol correction), never a
transport retry.

## Policy

- `--retry-policy bounded` (env `RUNSTEAD_RETRY_POLICY`) enables the retry
  orchestration for **configured compatible providers only**; the default is
  `off`, so existing workloads never gain implicit retries.
- Every retried physical attempt re-enters `Governor.Execute` with a new
  client request id (`-rN`), a new admission, new debit (attempts + retries
  ledgers) and its own durable provider attempt row.
- Bounds: the governor's existing `RetryBudget` (default 2), `TaskBudget`,
  elapsed task budget/deadline, rolling budgets, circuit and cooldown.

## Backoff order

1. authoritative `Retry-After` / reset observed on the response, when the
   governor selects a backoff from it (rate/capacity class today);
2. the durable `OperationalProfile` (#91) `cooldown_millis` input when it is
   larger (the profile is an INPUT only — it never executes retries and
   never changes governor authority);
3. the governor's own cooldown/circuit window remaining;
4. bounded deterministic governor baseline otherwise (already jittered).

Waiting respects `context.Context` (cancellation/deadline), reserves no
attempt budget, debits nothing until a physical attempt actually starts,
and never leaks timers (single-fire timers, always stopped).

## Durability / restart

- The original attempt, every retry, consumed budgets, cooldown and circuit
  state are persisted in the existing durable governor state (no new
  migration needed).
- A process that dies during backoff does not come back owning a free retry:
  retry orchestration is process-local; on restart nothing is replayed and
  no attempt is re-executed automatically (covered by tests reopening the
  SQLite state and asserting zero new physical requests).

## Verification

Deterministic unit tests (fake clock shared by governor and executor) cover
admission/accounting per retry, budget exhaustion, circuit open, auth/
permission/unknown/uncertainty never retried, cancel and deadline during
backoff without debit or dispatch, timer single-fire, and the
OperationalProfile cooldown input. E2E tests run the real CLI path against
provider-shaped `httptest` endpoints (429+Retry-After and transient 503 then
recovery) for all three compatible families and assert physical request
counts equal governed admissions and durable attempts/retries in SQLite.
