# Account Protection Governor Implementation Plan

> **For agentic workers:** implement task-by-task with tests first. The plan is
> intentionally limited to Issue #21 and its provider/config/trace seams.

**Goal:** Add a process-local, account-scoped ChatGPT Web request governor that
accounts every upstream attempt and fails closed on unsafe provider behavior.

**Architecture:** `internal/governor` owns an explicitly configured governor,
rolling ledgers, FIFO account lane, admission permit lifecycle, outcome
classification, cooldown/backoff and circuit state. It depends only on the
standard library plus the existing provider boundary. Optional `Clock`,
`Jitter`, `TelemetrySource` and `EventSink` seams make waiting and observation
deterministic without background workers. Provider safety metadata is validated
before admission; no OmniRoute adapter or agent loop is added.

**Tech Stack:** Go 1.22.2, standard library, existing `provider.Client` and
`trace` JSON logger.

## Global Constraints

- One personal ChatGPT Web request in flight per account; `max_in_flight` must be 1.
- Burst capacity is one; pacing is five seconds start-to-start.
- Instant defaults are 140/3h, reserve 20, 80/1h, 25/10m, task 80, retry 2, queue 16.
- Unknown/reasoning profiles require explicit ceilings and never inherit Instant.
- Every upstream attempt is debited at `Start`, including uncertain outcomes.
- No automatic retry loop, fallback, account/session/proxy/IP/model rotation or hidden provider policy.
- Local ceilings remain active when optional telemetry fails.
- No prompt, response, token, cookie, credential, personal account ID or raw HTTP body enters events.
- No SQLite, HTTP adapter, daemon, background worker or CLI request runner is added.

## File map

- Create `internal/governor/types.go`: profiles, config validation, admission/outcome/circuit/event/snapshot types, clock/jitter/telemetry/sink seams.
- Create `internal/governor/ledger.go`: timestamped rolling ledger and per-task counters.
- Create `internal/governor/governor.go`: lane, permit lifecycle, admission, pacing, telemetry, outcome/circuit updates and provider execution seam.
- Create `internal/governor/governor_test.go`: table-driven and deterministic concurrent tests for all Issue #21 invariants.
- Modify `internal/provider/provider.go`: executable route safety states and validation.
- Modify `internal/provider/fake.go` and `internal/provider/fake_test.go`: safe route metadata and unsafe fake coverage.
- Modify `internal/config/config.go` and `internal/config/config_test.go`: optional validated account-policy configuration without changing current CLI defaults.
- Create `internal/trace/policy.go` and `internal/trace/policy_test.go`: sanitized policy-event sink using the existing logger.
- Modify `README.md` and `docs/development.md`: link the SLO and state that the governor sits above adapters.
- Create `docs/account-protection.md`: technical SLO, profile, lifecycle, circuit and M1 limitation documentation.

## Implementation sequence

### Task 1: Lock provider safety and policy configuration behavior

1. Add failing tests for zero-value/unknown route safety rejection, explicit
   single-attempt safe metadata acceptance, all amplification flags being
   disabled, `max_in_flight != 1`, incomplete identifiers/ceilings and Instant
   defaults.
2. Run `go test ./internal/provider ./internal/governor ./internal/config` and
   observe failures because the governor package and safety metadata do not exist.
3. Implement the smallest explicit enums and `Config.Validate` required by the
   tests. Keep governor-specific types in `internal/governor`, with
   `provider.RouteSafety` as the provider-owned executable declaration.
4. Re-run the focused tests, then `gofmt`.

### Task 2: Add rolling ledgers and fake-time seams

1. Add tests for moving 10-minute, hourly and three-hour windows, efficient
   expiry at the boundary, per-task counts, model-pool separation and manual
   reserve subtraction.
2. Implement a deque-like timestamp ledger with an expiry head index and no
   goroutines. Expose only count/next-admission/snapshot operations needed by
   the governor.
3. Add fake-clock and deterministic-jitter test helpers in the governor test
   package; do not make production tests wait real seconds.
4. Run focused ledger tests and `go test -race` for the package.

### Task 3: Implement FIFO lane, permits and pacing

1. Add tests for one in-flight request, start-to-start pacing, response latency
   counting toward the interval, burst capacity, queue-full, cancellation
   without debit, cancellation without release burst, fairness quantum and no
   goroutine leaks.
2. Implement `Admit`/`TryAdmit`, `Permit.Start`, `Permit.CancelBeforeStart` and
   `Permit.Finish` with a mutex plus per-waiter notification channels. Inject
   timers through `Clock`; do not start scheduler goroutines.
3. Verify concurrent fake-provider execution and repeat the focused package
   tests with `go test -count=20 ./internal/governor/...`.

### Task 4: Add telemetry, retry accounting, outcome classification and circuit

1. Add table-driven tests for telemetry reducing but never raising ceilings,
   telemetry failure, Retry-After/reset precedence, deterministic 15/30/60/120
   jitter, task retry budgets, uncertain outcomes, security classifications,
   expired credentials, repeated pre-reset rate responses and the three-rate
   human acknowledgement threshold.
2. Implement optional `TelemetrySource` reads, conservative telemetry state,
   outcome finalization, `RetryEligibility`, explicit circuit transitions and
   acknowledgement. Do not create an autonomous retry loop.
3. Add `Execute` as the narrow provider integration seam: validate configured
   route safety, admit, start exactly once, call `provider.Client.Complete`,
   classify, finish and return sanitized result metadata.
4. Run focused tests and `go test -race ./internal/governor/...`.

### Task 5: Integrate config/trace and document boundaries

1. Add tests proving an optional policy in `config.Resolve` is validated while
   current workspace/log-level defaults remain unchanged. Add trace tests that
   parse policy JSON and assert no sensitive field can be emitted.
2. Add a small `trace.PolicySink` adapter over `slog`; policy itself must not
   import `slog`.
3. Link and review `docs/account-protection.md`, `README.md` and
   `docs/development.md` for accurate M1/#4/#7/#8/#13/#14/#16/#17 scope.

### Task 6: Full verification and delivery

Run, read and record all output from:

```bash
gofmt -w internal/governor/*.go internal/provider/*.go internal/config/*.go internal/trace/*.go
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/runstead
go test -count=20 ./internal/governor/...
bash experiments/protocol/test.sh
```

Inspect `git diff --check`, `git diff --stat`, the full diff and `git status`;
ensure `.omx/`, `CLAUDE.md` and unrelated files remain untracked/unmodified.
Commit with the Lore trailers, push `feat/issue-21-account-governor`, and
create a PR titled `Issue #21: implement adaptive account request governor`
whose body includes `Closes #21`, exact verification results and real M1
limitations.
