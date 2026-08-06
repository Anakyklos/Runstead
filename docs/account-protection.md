# ChatGPT Web Account Protection

## Purpose and SLO

Runstead's Account Protection SLO is an internal operating target for a
personal ChatGPT Web account used through an adapter such as OmniRoute. It is
not an SLA, an OpenAI guarantee, or evidence that unofficial ChatGPT Web
automation is permitted or safe. OpenAI's terms and product restrictions still
apply; throttling cannot make unofficial automation compliant, and Runstead
must not bypass limits or safeguards.

The M1 governor accounts 100% of observed upstream attempts, keeps one request
in flight per account, prevents bursts, preserves a manual-use reserve, and
fails closed on authentication or suspicious-activity signals. Account
protection has priority over task latency.

## Vocabulary

- A **client request** is one request submitted by Runstead to a provider
  adapter.
- An **upstream attempt** is one actual model request that may reach ChatGPT
  Web. M1 allows one attempt per protected provider completion. Executor
  retries must re-enter governor admission; internal retries, cooldown replay,
  pooling, fallback and combo routing are disabled in the M1 receipt route.
- An **account lane** is the FIFO queue and serialized execution state for one
  account policy.
- A **model pool** is the configured allowance bucket, such as Plus/Go Instant
  or a separate reasoning pool.

## Profiles and budgets

`PlusGoInstant` is an explicit local operating profile based on a published
160-message/3-hour family. Runstead defaults are 140 upstream attempts per
rolling 3 hours, an independent 20-attempt manual reserve, 80 per rolling
hour, 25 per rolling 10 minutes, 80 attempts per task, two recoverable retries
per task, queue capacity 16, a five-second minimum start-to-start interval and
one request in flight. These are Runstead ceilings, not limits approved or
guaranteed by OpenAI.

`Reasoning` and `Unknown` profiles do not inherit the Instant numbers. They
require explicit local rolling ceilings and an explicit manual reserve, keep a
separate model-pool ledger, and can use remaining/reset telemetry when it is
available. Successful recent calls never raise a local hard ceiling. The
manual reserve is tracked separately from automated consumption and is only
spent when the configured policy explicitly changes it.

Rolling ledgers use event timestamps and expire events relative to the current
time; they are not clock-aligned buckets. Local ceilings remain effective when
telemetry is absent or fails. Telemetry may reduce admission, but cannot make
it more permissive without an explicit configuration change.

For automated admission, the allowance headroom is the strictest known
constraint after preserving the reserve:
`min(local allowance remaining, trusted upstream remaining - manual_reserve)`.
The Instant local ceiling of 140 already represents the automated portion of
the published 160-message family, so the reserve is not subtracted a second
time from that rolling ledger. The 10-minute and 1-hour burst guards remain
independent local windows.

## Governor flow

Every possible model attempt follows:

```text
Admit -> Start -> provider.Client.Complete -> Finish
```

`Admit` acquires the account lane and waits for pacing, cooldowns, reset times,
telemetry and rolling/task/retry budgets. In the single-attempt route, `Start`
is the exact debit point. In the receipt-aware route, `Start` reserves the
logical request and `Finish` validates and debits one unit per authoritative
receipt. If the receipt authority is missing or invalid, the governor records
one conservative uncertain debit, marks telemetry unsafe and blocks later
admission. The governor never runs an autonomous retry loop; a future agent
loop must request each next attempt through `Admit` again.

`ClientRequestID` is a process-local identity for exact-request suppression: an
accepted ID cannot be admitted again, while cancellation before `Start` releases
the pending identity.

Every successful `Start` has exactly one terminal `Finish`; a late cancellation,
timeout, provider error or uncertain result still finishes with an uncertain
classification and keeps the debit. Only cancellation before `Start` can
remove a permit without an attempt.

The five-second interval is start-to-start. A response that takes five
seconds or longer already satisfies the next interval, while fast responses
and fast failures wait for the remaining interval. Cancellation while queued,
or after admission but before `Start`, consumes no attempt and does not reset
pacing or create a release burst.

The lane is FIFO, with a configurable task fairness quantum. If another task
is waiting, the current task yields after its quantum of consecutive starts to
the oldest different task still waiting in the lane.
There are no scheduler goroutines, daemon workers or distributed queues.

Configured event sinks use a process-local in-memory queue. Governor operations only
enqueue sanitized events; callers explicitly invoke `DrainEvents` to perform
sink I/O outside the account lane. All current admission, attempt and circuit
events are mandatory and remain queued until delivered; they are never
dropped. The agent `Executor` owns one drain after each execution, while direct
governor callers may drain explicitly. M1 does not create a background
dispatcher or a durable event queue, so a permanently blocked sink can grow
this process-local pending state; a future durable delivery/backpressure design
must preserve the mandatory-event invariant. There are currently no
best-effort event kinds.

## Route and receipt invariants

The provider boundary remains deliberately small: one
`provider.Client.Complete` invocation represents one logical completion and
owns no quota, account rotation or scheduling policy. The legacy route must
explicitly guarantee one attempt and disable every amplification mode. The M1
receipt route may report internal retries and cooldown replays, but must declare
account pooling, automatic fallback and combo routing disabled. Unknown or
unsafe declarations are rejected before queue admission.

The #4 OmniRoute adapter remains fail-closed until its management evidence and
receipt transport are verified. `Preflight` may validate observable resilience
and route settings, but it never authorizes execution by itself. An M1 receipt
set must contain one attempt using one provider, one concrete model and one
account-lane hash. `ModelPool` is only an allowance bucket and is never
accepted as the model identity. Attempt IDs are retained for three hours with
a bounded in-memory cap, timestamps must fit the real `Start` to `Finish`
interval, and pacing resumes from the authoritative attempt start.

The adapter still checks `/api/settings`, `/api/models/alias`,
`/api/settings/model-aliases`, `/api/fallback/chains`, `/api/combos`,
`/api/model-combo-mappings` and `/api/providers` as fail-closed observable
sanity checks. It rejects absent settings evidence, aliases for the configured
model, wildcards, fallback chains, combos, model-to-combo mappings or more
than one active connection for the configured provider. It does not infer
safety from model-name markers. Transport and response parsing remain covered
through an internal test seam, not a production authorization path.

Receipt outcomes are typed for rate/capacity, authentication, HTTP 403,
login challenges, CAPTCHA, suspicious activity, account warnings, feature
restrictions, transport uncertainty and upstream failures. The governor
reconciles every receipt outcome before applying the logical completion, so a
later success cannot erase an earlier security or rate signal from the circuit.

## Telemetry and circuit breaker

The optional OmniRoute telemetry source reads `/api/rate-limits` and
`/api/resilience`, carrying only sanitized equivalents of remaining allowance,
reset time, cooldown, rate/capacity state and an upstream breaker. Malformed
or unavailable optional telemetry returns an unhealthy source signal; it never
replaces or disables the governor's local limits. It does not export OmniRoute
HTTP types. `Retry-After` and reliable reset
times take precedence over local jittered backoff. Without authoritative
guidance, the recorded backoff sequence is 15s, 30s, 60s and 120s with
injectable jitter whose baseline is a floor; selecting a backoff never performs
a retry.

The circuit has explicit `closed`, `open_until` and
`human_review_required` states. Authentication denial, HTTP 403 equivalents,
login challenges, CAPTCHA, suspicious activity, account warnings and feature
restrictions open it for human review immediately. An expired credential
blocks model requests and exposes only a separate, single credential-refresh
seam; the refresh path is not a model attempt. A second rate/capacity response
before the previous reset opens the circuit through that reset plus five
minutes. Three rate/capacity responses in a rolling hour require explicit
human acknowledgement. Severe circuits do not reopen automatically and no
classification selects another account, session, proxy, IP, provider or
model.

The adapter uses one non-streaming POST per `Complete` call, with no adapter
retry, cooldown wait, fallback, account rotation, pooling, pacing or model
swap. Its typed errors classify transport, timeout/cancellation, authentication,
403, rate/capacity, login challenge, CAPTCHA, suspicious activity, account
warning, feature restriction, connection reset, empty/malformed response and
upstream failure; `omniroute.Classify` maps those errors into governor
outcomes. Events and snapshots contain only policy/account identifiers, provider/model
pool, profile, task and client request identifiers, attempt sequence,
admission/result codes, budget summaries, telemetry summaries, backoff,
cooldown and circuit transitions. Prompts, responses, tokens, cookies,
credentials, personal account identifiers and raw HTTP bodies are excluded.

## M1 state boundary and future work

Ledger, task, lane, request-identity and circuit state are process-local and
exposed through sanitized snapshots. Completed request identities are retained
for three hours with a bounded in-memory cap, while pending and active
identities are never pruned; task state is likewise bounded and protected
while queued or active. Restart-safe cooldown, ledger and identity persistence
is deferred to #8; M1 does not pretend that restarting Runstead preserves
protection.
The #4 OmniRoute adapter reuses this governor. #7 must route every agent turn
through it. #13 adds stress and failure evidence, #14 publishes
consumption/SLO evidence, and #16/#17 reuse
the same account policy without an independent provider quota policy. #8
provides durable state; direct-connector work and any later transport changes
remain behind the same boundary.
