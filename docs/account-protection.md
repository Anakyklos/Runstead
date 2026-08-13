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
- An **allowance kind** (#58) is the typed upstream allowance semantic:
  `published_quota`, `unlimited_text` or `unknown`. It answers "does the
  upstream surface publish a numeric text allowance?" and is independent from
  Runstead-local workload controls, which are always active.
- A **profile** is the historical allowance identifier (`plus_go_instant`,
  `reasoning`, `unknown`, `luna_unlimited_text`) that maps to exactly one
  allowance kind and is what the durable projection stores.

## Two independent layers

The governor separates two concerns that must never be conflated:

1. **Upstream allowance policy**: optional numeric rolling quotas and a
   profile-specific manual reserve, applied only when the upstream publishes
   or exposes a numeric text allowance (`published_quota`).
2. **Account-safety/workload policy**: always-active serialization
   (`max_in_flight = 1`), start-to-start pacing, per-task attempt budgets,
   retry budgets, bounded queue/fairness, cooldown handling, rate/capacity
   handling, circuit breakers, authentication/challenge/security fail-closed
   behavior, authoritative attempt-receipt accounting, hidden-amplification
   protection and durable governor state.

A model advertised as unlimited text may omit the first layer; it never omits
the second. A numeric allowance is not account safety, and "unlimited" is not
"ungoverned".

## Allowance kinds

| Kind | Meaning | Numeric rolling quota | Manual reserve |
| --- | --- | --- | --- |
| `published_quota` | Upstream publishes/exposes a numeric text allowance | Enforced (must be positive, 10m < 1h < 3h) | Profile-specific; never consumed automatically |
| `unlimited_text` | Explicitly configured (or observed through trustworthy evidence) unlimited text | Absent; fabricating one fails validation | Absent; configuring one fails validation |
| `unknown` | No evidence either way; upstream allowance unknown | Absent as an upstream claim; explicit conservative local ceilings are required and enforced (#21 contract) | Local manual-use reserve; required as part of the local layer (#21 contract) |

`plus_go_instant` and `reasoning` are `published_quota` profiles.
`luna_unlimited_text` is the explicit unlimited-text profile.
`unknown` is the no-evidence profile and must never silently become unlimited
because requests keep succeeding: admission stays governed by the local
workload layer, and success never upgrades the allowance semantic.

The allowance kind is derived from the persisted profile on restore, so legacy
projections survive without a schema migration. Changing the allowance kind
between runs never resets the durable rolling ledger, task attempts, retry
accounting, circuit state, cooldown state, receipt-replay protection or
safety/unsafe telemetry state: the configured policy is authoritative for
admission, and the durable accounting carries over unchanged.

## Profiles and budgets

`PlusGoInstant` is an explicit local operating profile based on a published
160-message/3-hour family. Runstead defaults are 140 upstream attempts per
rolling 3 hours, an independent 20-attempt manual reserve, 80 per rolling
hour, 25 per rolling 10 minutes, 80 attempts per task, two recoverable retries
per task, queue capacity 16, a five-second minimum start-to-start interval and
one request in flight. These are Runstead ceilings, not limits approved or
guaranteed by OpenAI.

`Reasoning` is a separate `published_quota` profile: it requires explicit
local rolling ceilings and an explicit manual reserve, keeps a separate
model-pool ledger, and can use remaining/reset telemetry when it is available.
Successful recent calls never raise a local hard ceiling. The manual reserve
is tracked separately from automated consumption and is only spent when the
configured policy explicitly changes it.

`LunaUnlimitedText` is the explicit unlimited-text profile (#58). It has no
invented 160/3h rolling quota and no inherited 20-message manual reserve.
"Unlimited text" means no known numeric upstream text-message quota; it never
means unlimited Runstead execution, and it does not make ChatGPT Web an
API-equivalent service. The profile becomes active only through explicit
operator configuration (`--allowance-profile luna_unlimited_text` or
`RUNSTEAD_ALLOWANCE_PROFILE`) or trustworthy observed/configured evidence as
required by #58. Model naming alone is never evidence: a model called
"unlimited" or "Luna" does not activate the profile, and no account is probed
to discover limits. Live rollout verification is an opt-in manual procedure
(see below) that never intentionally drives the account into a restriction.

`Unknown` is the no-evidence profile: the upstream allowance is unknown, so
the conservative local layer remains mandatory exactly as in the #21 contract.
Explicit positive local rolling ceilings and a local manual-use reserve are
required and enforced as Runstead workload protections, not as upstream
allowance claims; `DefaultUnknownConfig` supplies the same conservative local
family as the Instant profile as a reviewed starting point. Observed
rate/capacity/cooldown/reset telemetry still constrains admission, and
repeated successful calls never promote Unknown to unlimited or relax the
local ceilings.

Rolling ledgers use event timestamps and expire events relative to the current
time; they are not clock-aligned buckets. Local ceilings remain effective when
telemetry is absent or fails. Telemetry may reduce admission, but cannot make
it more permissive without an explicit configuration change.

For automated admission under a `published_quota` allowance, the headroom is
the strictest known constraint after preserving the reserve:
`min(local allowance remaining, trusted upstream remaining - manual_reserve)`.
The Instant local ceiling of 140 already represents the automated portion of
the published 160-message family, so the reserve is not subtracted a second
time from that rolling ledger. The 10-minute and 1-hour burst guards remain
independent local windows. Under `unlimited_text` no reserve exists and none
is subtracted. Under `unknown` the local manual-use reserve remains part of
the conservative local layer and is still subtracted from observed upstream
remaining, exactly as in the #21 contract. An observed remaining counter is a
restriction-only signal under `published_quota` and `unknown` (it can never
expand admission) and is deliberately not a text-allowance signal under
`unlimited_text`, while `Retry-After`, cooldown, rate-limited,
capacity-exhausted and explicit-reset signals remain authoritative under every
kind.

## Opt-in live observation procedure

The announced Luna unlimited-text rollout is an upstream product transition
whose operational behavior must be measured, not assumed. Activating
`luna_unlimited_text` is opt-in and requires the operator to first collect
sanitized evidence that distinguishes:

- Luna actually being the model used by the configured ChatGPT Web route;
- plain text turns from file/image/tool-backed turns;
- absence of the old fixed text-message allowance for the account/plan under
  test;
- rate/capacity or temporary restriction signals that still occur under
  "unlimited" text;
- whether remaining/reset telemetry disappears, changes shape or remains
  useful;
- whether Free and Go behave differently despite sharing the announcement.

This procedure is manual and disabled by default. It must never probe limits
by intentionally driving the account until a restriction occurs, and it must
never be automated into a calibration loop. If the upstream behavior cannot be
identified reliably, keep the profile `unknown` or explicitly configured and
fail closed rather than guessing.

## Governor flow

Every possible model attempt follows:

```text
Admit -> Start -> provider.Client.Complete -> Finish
```

`Admit` acquires the account lane and waits for pacing, cooldowns, reset times,
telemetry and rolling/task/retry budgets. In the single-attempt route, `Start`
is the exact debit point. In the receipt-aware route, `Start` reserves the
logical request and `Finish` validates and debits one unit per authoritative
receipt. If a valid receipt set violates the M1 one-attempt policy, every unseen
receipt is still reconciled before the governor marks telemetry unsafe and
blocks later admission. If receipt authority is missing or structurally
invalid, the governor records one conservative uncertain debit, marks telemetry
unsafe and blocks later admission. The governor never runs an autonomous retry
loop; a future agent loop must request each next attempt through `Admit` again.

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
receipt route must declare account pooling, automatic fallback, combo routing,
internal retries and cooldown replays disabled. A structurally valid receipt set
that violates this policy is fully reconciled and then blocks the lane. Unknown
or unsafe declarations are rejected before queue admission.

The #4 OmniRoute adapter remains fail-closed until its management evidence and
receipt transport are verified. `Preflight` may validate observable resilience
and route settings, but it never authorizes execution by itself. An M1 receipt
set must contain one attempt using one provider, one concrete model and one
account-lane hash. `ModelPool` is only an allowance bucket and is never
accepted as the model identity. Attempt IDs are retained for three hours with
a bounded in-memory cap, timestamps must fit the real `Start` to `Finish`
interval, and pacing resumes from the authoritative attempt start normalized to
the local permit interval. Remote timestamp values remain audit data and never
move rolling-window expiry earlier than the local call.

The adapter still checks `/api/settings`, `/api/models/alias`,
`/api/settings/model-aliases`, `/api/fallback/chains`, `/api/combos`,
`/api/model-combo-mappings` and `/api/providers` as fail-closed observable
sanity checks. It rejects absent settings evidence, aliases for the configured
model, wildcards, fallback chains, combos, model-to-combo mappings or more
than one active connection for the configured provider. It does not infer
safety from model-name markers. Transport and response parsing remain covered
through an internal test seam, not a production authorization path.

### Gateway-contract health

The on-demand `gateway_contract_health` probe is a read-only signal about the
OmniRoute **management gateway contract** only. It reads exactly
`/api/providers`, `/api/settings` and `/api/models/alias` with bounded GETs. It
does not measure ChatGPT Web, Sentinel or any private upstream endpoint, and it
does not prove authoritative attempt accounting.

Its typed states are `unknown`, `healthy`, `degraded` and
`protocol_changed`. A new client starts at `unknown`; timeouts, cancellation,
transport uncertainty, malformed JSON and structural shape drift never become
`healthy`. Configuration concerns (active-connection selection, `defaultModel`
or aliases) never affect the health state: they are decided by `RouteSafety`
and preflight, not by this probe. All non-healthy states block protected
execution. `healthy` only means that the three management responses have
recognized compatible shapes. It does not replace `RouteSafety`, governor
admission, policy, circuits, budgets, receipts or durable delivery-state
accounting. The latest sanitized result is exposed in the existing trace path
as `gateway_contract_health`; raw management bodies and credentials are never
included.

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

Missing numeric remaining counters on an unlimited-text profile are never
interpreted as infinite positive telemetry: they are simply not a numeric text
allowance signal. Where the dashboard still exposes a remaining counter under
unlimited text, it is tracked for observability but does not gate text
admission; under `published_quota` and `unknown` an observed remaining counter
can only restrict admission, never expand it. Malformed or unavailable
telemetry still returns an unhealthy source signal and fails closed exactly as
before. A product allowance (including an advertised "unlimited" one) is never
an API authorization: it does not permit bypassing provider safeguards and
does not remove provider-risk or terms considerations, and Runstead does not
use ChatGPT Web as a resale or general public API.

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
