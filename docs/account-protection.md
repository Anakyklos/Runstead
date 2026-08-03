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
  Web. Hidden provider retries are additional upstream attempts and are not
  allowed in the protected M1 route.
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
telemetry and rolling/task/retry budgets. `Start` is the exact debit point:
the attempt is added to the account/model-pool and task ledgers only when the
provider call is about to begin. `Finish` releases the lane, classifies the
outcome, updates cooldown/circuit state and returns retry eligibility. The
governor never runs an autonomous retry loop; a future agent loop must request
each next attempt through `Admit` again.

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

## Single-attempt invariant

The provider boundary remains deliberately small: one
`provider.Client.Complete` invocation represents at most one upstream model
attempt and owns no retry, quota, fallback, account rotation or scheduling
policy. M1 additionally requires executable route-safety metadata: the route
must explicitly guarantee single-attempt behavior and declare internal retry,
cooldown replay, account pooling and automatic fallback disabled. Unknown or
unsafe metadata is rejected before queue admission. The OmniRoute adapter is
not implemented by this issue.

## Telemetry and circuit breaker

The optional telemetry seam carries sanitized equivalents of remaining
allowance, reset time, cooldown, rate/capacity state and an upstream breaker.
It does not import OmniRoute HTTP types. `Retry-After` and reliable reset
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

Events and snapshots contain only policy/account identifiers, provider/model
pool, profile, task and client request identifiers, attempt sequence,
admission/result codes, budget summaries, telemetry summaries, backoff,
cooldown and circuit transitions. Prompts, responses, tokens, cookies,
credentials, personal account identifiers and raw HTTP bodies are excluded.

## M1 state boundary and future work

Ledger, task, lane and circuit state are process-local and exposed through
sanitized snapshots. Restart-safe cooldown and ledger persistence is deferred
to #8; M1 does not pretend that restarting Runstead preserves protection.
Issue #4 can add the OmniRoute adapter and telemetry only by reusing this
governor. #7 must route every agent turn through it, #13 adds stress and
failure evidence, #14 publishes consumption/SLO evidence, and #16/#17 reuse
the same account policy without an independent provider quota policy. #8
provides durable state; direct-connector work and any later transport changes
remain behind the same boundary.
