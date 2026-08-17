# Standalone first-party browser substrate spike

**Date:** 2026-08-16 (live run); 2026-08-17 (review-driven hardening + canonical evidence rebuild)
**Status:** Research spike, disposable prototype. No production integration.
**Issue:** Refs #16 (research only; issue not closed)
**Branch:** `research/issue-16-standalone-browser-spike`
**Base:** `188048a00a0089aab6f6419f18d5269e5dac2680` (main at spike start)
**Live model turn budget used:** 2 of 2 (one success turn, one post-dispatch
cancellation turn that aborted). Zero additional model turns were consumed by
the review hardening (everything below is deterministic rebuild, fixtures, and
dry-mode validation).

---

## Motivation

PR #72 proved that a browser boundary is technically viable for a first-party
ChatGPT Web attempt: the authenticated session stayed in the browser profile,
Runstead never saw credentials, one live turn produced exactly one physical
model-effect POST, pre-dispatch cancellation produced N=0, and the final
response was correlated. But the prototype depended entirely on the Orca
runtime/CLI/profile. Without Orca it does not run, so the standalone
Runstead-owned substrate remained **UNPROVEN**.

This second spike answers the remaining question with evidence: *can
Runstead own and control a browser substrate directly — dedicated profile,
transport-level network observability, cancellation, crash handling — with
no dependency on Orca, JCode, OmniRoute or any other agent runtime?* The
candidate tested here is Chromium/Chrome + CDP, because CDP closes the
specific gap left by PR #72: Network-domain visibility at the browser level
(no fetch/XHR monkey-patching), explicit target/browser lifecycle, and
observable cancellation/disconnect.

This is a disposable spike. It does not implement `provider/chatgptweb`, does
not change `runstead run`, does not touch the governor, state, verifier,
policy or OmniRoute, and does not modify any production package.

## Relation to PR #72

| Aspect | PR #72 (Orca embedded browser) | This spike (standalone Chrome + CDP) |
|---|---|---|
| Runtime dependency | Orca runtime + CLI + profile | none (Node built-ins + Chrome) |
| Network observability | fetch/XHR page wrapper + resource timing | CDP Network domain (transport level) |
| Browser process ownership | Orca | spike (spawn + DevToolsActivePort) |
| Profile | Orca's embedded-browser profile | spike-owned dedicated profile |
| Cancellation pre-dispatch | N=0 | N=0 |
| Cancellation post-dispatch | UNPROVEN (response completed before click) | aborted (`net::ERR_ABORTED`, canceled) |
| Crash/disconnect | not exercised | SIGKILL -> typed fail-closed, no replay |

The concepts reused (not the code): fail-closed DOM contract with composer
fingerprinting, turn-scoped send counting, DOM terminal markers, sanitized
evidence.

## Scope / non-goals

In scope:

- own the full browser lifecycle (profile, launch, CDP discovery, target
  discovery/attach, navigation, DOM interaction, cancellation, shutdown)
  without Orca/JCode/OmniRoute;
- transport-level network accounting via CDP Network events only;
- one bounded success turn (`Count from 1 to 80 ... RUNSTEAD_STANDALONE_OK`);
- one post-dispatch cancellation turn (budget 2/2, no automatic retry);
- pre-dispatch cancellation (N=0), crash/disconnect classification, and a
  fail-closed page-contract matrix (0 additional model turns);
- sanitized evidence artifacts and this document.

Non-goals (explicitly forbidden by the task):

- no production `provider/chatgptweb`, no CLI changes, no governor/state/
  verifier/policy changes;
- no `Closes #16`, no `Closes #29`, no `Closes #7`;
- no #17, #41, #57 work;
- no Orca/JCode usage of any kind (absolute gate);
- no more than 2 live model turns;
- no automated login/MFA/CAPTCHA, no account rotation, no fallback, no hidden
  retries;
- no credential capture/export/persistence; no profile copying;
- no Camoufox/Firefox-Juggler implementation (comparison only);
- no @Graph Mode.

## Runtime ownership

The harness (`experiments/first-party-chatgpt-web-standalone/`) is plain
Node.js (v24.19.0) using only built-ins: `child_process`, `fs`, `fetch` and
the built-in `WebSocket` client. There are no npm packages, no `go.mod`
changes, no Orca/JCode/OmniRoute imports or invocations.

Proof of zero external runtime:

- source scan of the harness for spawn/exec calls: the only long-lived child
  process is Chrome (`lib/browser.mjs`); the only `execFileSync` calls are
  momentary `which`/`ps` probes (`evidence/environment.json`);
- self-audit scan for forbidden credential APIs: 0 findings
  (`evidence/environment.json`);
- the harness was developed and run without any Orca/JCode process, and its
  operation was validated both before and after the Orca-dependent PR #72
  artifacts existed in the repo (the spike never references them).

## Browser/profile lifecycle

1. **Binary discovery**: `RUNSTEAD_SPIKE_CHROME` env override, else
   `which google-chrome-stable` (found `/usr/bin/google-chrome` ->
   `/opt/google/chrome/chrome`, Chrome 151.0.7922.71, ~431 MB).
2. **Profile**: dedicated directory `profiles/standalone-spike` inside the
   experiment (gitignored). Created fresh by the harness, never copied from
   any other profile, never shared. The profile holds the real auth session.
3. **Launch**: `--user-data-dir=<profile> --remote-debugging-port=0
   --no-first-run --no-default-browser-check`. Port 0 lets the OS assign the
   port; the harness reads `<profile>/DevToolsActivePort` (spike-owned
   discovery). Stale owners of the same profile are terminated first.
4. **CDP endpoint**: `http://127.0.0.1:<port>/json/version` ->
   browser websocket URL (never logged), plus `/json/list` for targets.
5. **Target discovery/attach**: browser-level connection,
   `Target.setDiscoverTargets`, `Target.createTarget` for
   `https://chatgpt.com` when absent, `Target.attachToTarget` (flattened)
   -> per-page session for Network/Runtime/Input/Page.
6. **Shutdown**: SIGKILL in the crash test; SIGTERM on normal close. The
   profile persists for reuse.

All ten ownership responsibilities from the task are implemented in the
harness (see the ownership map in `run_spike.mjs` header).

**Measured login-gate property (new finding).** OpenAI's login gate —
Cloudflare Turnstile interstitial on `auth.openai.com` plus Auth0's
"browser or app may not be secure" — challenges Chrome launches that carry
remote-debugging flags, and the challenge never resolves (stuck on
"Verificação bem-sucedida. Esperando a resposta de auth.openai.com"). A
fully clean launch (no flags) logs in normally. This was measured three
times on 2026-08-16 (with `--remote-allow-origins`, with only
`--remote-debugging-port`, and clean). The spike therefore uses a
**two-phase enrollment**: a clean launch for the user-assisted login, then a
CDP launch of the same profile for the runtime. No automation concealment
was used (no UA spoofing, no `--disable-blink-features=AutomationControlled`).
This is a transport property to be measured and weighed in the ADR, not a
reason to pick stealth tooling (see Candidate comparison).

## Authentication custody

- Login is manual in the visible enrollment window (email/password, MFA as
  usual). The harness never receives, reads or stores a password.
- The harness never calls cookie/localStorage/sessionStorage APIs, never
  reads response bodies, never exports anything from the profile. The CDP
  method allowlist used is `Target.*`, `Page.enable`, `Runtime.*`,
  `Input.insertText`, `Network.enable`; methods like
  `Network.getCookies`/`Network.getResponseBody`/`Storage.*` are absent by
  construction (`evidence/auth-custody.json`).
- Session persistence is verified after enrollment by relaunching the same
  profile with CDP and observing the page contract `ready` (composer
  present, not signed out) — no credential material involved.
- Runstead SQLite is never touched by the spike.
- If the session expires, the contract reports `login_required` and the
  harness fails closed; it does not attempt to repair auth.

## CDP boundary

Single WebSocket to the browser-level endpoint; flattened per-target
sessions for the ChatGPT page. Commands used: `Target.setDiscoverTargets`,
`Target.getTargets`, `Target.createTarget`, `Target.attachToTarget`,
`Target.closeTarget`, `Page.enable`, `Runtime.enable`,
`Runtime.evaluate`, `Input.insertText`, `Network.enable`. Events consumed:
`Network.requestWillBeSent`, `Network.responseReceived`,
`Network.dataReceived`, `Network.loadingFinished`, `Network.loadingFailed`,
`Target.targetCreated/Destroyed/Crashed/Detached`. No `Fetch.enable`
(no request interception), no `Network.getResponseBody`.

## Transport-level accounting

Authoritative source: CDP Network events on the page session. No page-level
wrappers are injected (unlike PR #72). Classification (full path, method):

| Class | Rule |
|---|---|
| `model_effect_conversation` | POST `/backend-api/f/conversation` or `/backend-api/conversation` |
| `model_effect_prepare` | POST `/backend-api/f/conversation/prepare` (known pre-dispatch step) |
| `potential_model_effect` | ANY other POST under `/backend-api/conversation/*` or `/backend-api/f/conversation/*` (unknown continuation/resume/replay candidates; BLOCKS the no-hidden-retry verdict) |
| `session_check` | GET `/backend-api/me`, `/backend-api/accounts/*`, settings |
| `sentinel_aux` | `/backend-api/sentinel/*` |
| `ces_telemetry` | `/ces/*` |
| `conversation_list` | `/backend-api/conversations` |
| `conversation_api_aux` | GET `/backend-api/conversation/*` (stream_status, textdocs) + known POSTs `init`/`prepare` |
| `backend_api_aux` | other `/backend-api/*` |
| `static_asset` | `/_next/*`, `/static/*`, etc. |
| `other` | everything else (static assets, fonts, avatars, third-party noise) |

Classification is conservative by design: the two exact model-effect paths,
the `prepare` pre-dispatch step and the `init` aux path are allowlisted; any
other POST in the conversation namespace is `potential_model_effect`
(uncertain) and flips `hidden_retry_or_fanout` from `false` to `true` instead
of passing silently as auxiliary. In the observed live run there were zero
such unknowns (the only conversation-namespace POSTs were `prepare`, `init`),
so the clean verdicts below hold under conservative accounting.

Every request is recorded with sequence, timestamp, method, host (hostname
only), path (pathname only, query dropped, >160 chars truncated), opaque
truncated request id, type, window (baseline/turn/post_turn) and turn id;
then status, mimeType, completion (`finished`/`failed`), errorText, and
cumulative SSE bytes. Headers, cookies, bodies and query strings are never
read or logged (structural allowlist in `lib/network.mjs`).

**Observed turn windows.** Baseline (pre-dispatch, ~3 s quiet): 0
model-effect sends (341 `other`, 12 `ces_telemetry`, 30 `backend_api_aux`,
4 `session_check`, 2 `sentinel_aux`, 1 `conversation_list`, 1
`conversation_api_aux`, 4 `model_effect_prepare`). The `prepare` calls occur
as the send action is armed, before the conversation POST.

## One-turn lifecycle

Turn 1 (success, budget 1/2):

1. contract `ready`; composer focused; prompt inserted via `Input.insertText`;
   composer fingerprint verified (length 79, FNV-1a hash matches) — fail
   closed on mismatch;
2. dispatch click; turn window opened;
3. `POST /backend-api/f/conversation` (requestId `989287.559`) — 1 physical
   send;
4. `responseReceived` 200 `text/event-stream`; 15 `dataReceived` chunks;
5. `loadingFinished` (12017 bytes);
6. DOM: terminal markers present, `busy=false`, last assistant message
   contains `RUNSTEAD_STANDALONE_OK`, conversation id in URL;
7. no model-effect request in baseline, post-turn, or beyond turn 1: no
   hidden retry/fan-out observed.

## Final-turn correlation

Signals used for turn 1 correlation, labeled by level:

- **transport-level**: exactly one in-window `POST /backend-api/f/conversation`
  (status 200, SSE, streamed, finished); zero conversation POSTs in the
  baseline or post-turn windows; same single page target throughout;
- **DOM-level**: terminal markers, `busy=false`, expected token in the last
  assistant message, conversation id from the URL (`6a811a31-6f7…`);
- **inference**: the dispatch click immediately precedes the request
  (request `willBeSent` ~69 ms after click), and the conversation id from
  the DOM matches the turn's timing window.

No response body was ever read; the token check is DOM-level only.

## Cancellation

**Pre-dispatch (0 model turns).** Text is inserted into the composer and
then cleared without ever clicking send; the turn window records
`physical_sends: 0`, `prepare_sends: 0`, `model_effect_in_post: 0`
(`output/summary-dry.json`, dry run).

**Post-dispatch (turn 2, budget 2/2).** Dispatch confirmed at the transport
level: the in-turn conversation POST became visible to the Network domain
(`sent_confirmed`) and the stop button was clicked immediately **without
waiting for the first response byte**. In the live run: `POST
/backend-api/f/conversation` (requestId `989287.643`) 43 ms after the
dispatch click; stop located and clicked 61 ms after dispatch (18 ms after
the request was sent); `Network.loadingFailed` with `net::ERR_ABORTED`,
`canceled: true` — the request was aborted before any SSE chunk
(`response_started` was not observed for this request). No retry, no
re-dispatch; the DOM settled with `busy=false` and the turn-2 assistant
section never rendered.

Classification: `sent_confirmed -> canceled_aborted` (response_started not
reached). This is a positive, observable cancellation: a physical
model-effect request was in flight and was aborted by the stop action. The
current harness reproduces exactly this semantics: `waitForSent` (the
conversation POST's `requestWillBeSent`), stop click, then `loadingFailed`
observation. It NEVER waits for response data before cancelling, so the code
is aligned with what was actually executed in the live run (which was
executed by harness v1 with the same sent-confirmed flow). The criterion is
marked PROVEN with the caveat that the abort landed before the first response
byte; a later abort (mid-stream) was not measured.

**Timeout (0 additional model turns, deterministic dry proof).** Issue #16's
prototype requirements include proving cancellation **and timeout** behavior.
The timeout behavior is proven synthetically in
`evidence/fail-closed-proofs.json` (section `timeout_fail_closed`, run by
`run_spike.mjs dry`): a turn whose model-effect request is SENT but never
starts (and never completes) within the bounded wait must derive the typed
fail-closed state `sent_timeout_fail_closed` and must NOT re-dispatch or
replay (`replay: false`, `retry_dispatched: false`); a later unknown
conversation-namespace POST flips the no-hidden-retry verdict to blocked.
The runtime mirrors this: every bounded wait in `run_spike.mjs` (response
start, terminal DOM, cancel outcome, sent confirmation) is wrapped in
`waitForTyped`, which on expiry records a typed `uncertain_timeout` event
with `re_dispatched: false` and terminates the turn fail-closed (recovery
belongs to Runstead, never the browser). Zero model turns.

## Crash/disconnect

Without consuming a model turn, the browser was SIGKILLed while the CDP
connection and the page session were live:

- CDP websocket closed abnormally (close code 1006, `clean=false`);
- browser process exited with signal SIGKILL;
- zero new network records after the kill; the harness never re-dispatched
  and never replayed history (recovery of the task belongs to Runstead, not
  the browser);
- typed state: `fail_closed_no_replay` (recorded in both the live and dry
  runs).

Additionally, `Target.closeTarget` on the fixture target produced
`target_destroyed` (target lifecycle observable).

## Page-contract drift

Before any interaction the harness probes a self-contained readiness
expression: origin must be EXACTLY `https://chatgpt.com` (a real origin
comparison via `location.origin === 'https://chatgpt.com'`, NEVER a textual
prefix — a lookalike such as `https://chatgpt.com.evil.example` fails
closed), no signed-out markers (`Log in`/`Sign up`/`Entrar`/`Criar
conta`/`Cadastre-se`), no blocking dialog, composer known (`#prompt-textarea`
or the localized contenteditable variants). Auth origins are an explicit
enumeration (`https://auth0.openai.com`, `https://auth.openai.com`,
`https://platform.openai.com`) and nothing else may satisfy `auth_pending`.
Verdicts: `ready`, `login_required`, `auth_pending`, `dialog_blocking`,
`contract_missing`. Anything unknown fails closed — no clicks, no typing, no
dispatch.

The exact-origin rule, the conservative classifier, url/conversation-id
redaction and the timeout state machine are all covered by deterministic
fixtures in `evidence/fail-closed-proofs.json` (10 origin fixtures including
`chatgpt.com.evil.example`, `www.chatgpt.com`, `sub.chatgpt.com`,
insecure-scheme and auth-lookalike cases; 11 classifier fixtures including
unknown conversation-namespace POSTs; 3 URL-shape fixtures; 6 timeout
state-table rows). Zero model turns.

Live fixture proof (0 model turns): a local `file://` fixture mimicking a
signed-out page with a composer is classified `contract_missing` (wrong
origin) and the harness refuses to interact (`fixture_no_dispatch`), even
though it contains a fake `Log in` button and a fake composer
(`output/summary-dry.json`). The login wall of real chatgpt.com is
classified `login_required` (pt-BR labels supported).

## Security/redaction

- No credential capture, no cookie/localStorage access, no response bodies,
  no header logging (structural allowlists in `lib/network.mjs`);
- structural redaction applied before any stdout/JSON/file: hex tokens
  (UUIDs, conversation ids) truncated to 8 chars, long opaque tokens
  truncated, home paths rewritten to `~` (`lib/sanitize.mjs`);
- **conversation ids are NEVER persisted, not even truncated**: evidence
  carries a non-correlatable placeholder only (`conv#turn1`, `conv#turn2`)
  with `conversation_id_redacted: true` (`conversationIdEvidence` in
  `lib/sanitize.mjs`). Conversation ids that appear inside API paths
  (`/backend-api/conversation/<id>/stream_status`) are replaced by a
  placeholder segment (`<conv>`);
- query strings and fragments are always dropped (`urlShape`); target URLs
  are never logged raw — `Target.targetCreated` records host plus a coarse
  first-segment class (`targetShape`), so conversation id, query and fragment
  can never leak through target events;
- **per-request evidence is minimized**: `other` traffic (static assets,
  fonts, avatars, third-party noise) is dropped from the persisted
  per-request artifacts (its aggregate counts are kept), and the persisted
  lifecycle excludes `other` request streams;
- **profile paths are hermetic**: evidence never records an absolute
  filesystem path; the dedicated profile is expressed repo-relative as
  `profiles/standalone-spike` (and raw v1 key events are normalized to this
  form on canonicalization), so committed evidence is reproducible and does
  not leak a machine-specific checkout path;
- self-audit: the harness scans its own source (comments stripped) for
  forbidden APIs — 0 findings (`evidence/environment.json`,
  `evidence/auth-custody.json`);
- the dedicated profile is gitignored and never committed; no secrets in
  git, none in SQLite.

The redaction rules above are enforced by deterministic fixtures in
`evidence/fail-closed-proofs.json` (URL-shape, target-shape and
conversation-id sections). A generic redactor is NOT trusted for
conversation ids (it can leave an 8-char prefix); the placeholder mechanism
is the only path conversation ids take into evidence.

**Reusable validation entry point.** `experiments/first-party-chatgpt-web-standalone/test.sh`
runs the deterministic suite: fail-closed proofs, conservative-classifier and
conversation-path edge cases, URL/target/file/conv-id redaction shaping, the
canonical live-evidence rebuild idempotence, cross-artifact agreement, and a
leak scan over the derived artifacts (including a hermeticity assertion that
no absolute filesystem path leaks into evidence). Zero browser launch, zero
model turns.
It is wired into the repository CI (`.github/workflows/ci.yml`,
"Run standalone browser-substrate spike deterministic checks") so the
review-hardened evidence invariants are continuously validated on the branch.
The `urlShape`/`sanitizeConversationPath` helpers also replace any UUID-shaped
segment (including the `/c/<conversation-id>` content route) with a
placeholder before the generic redactor runs, so no truncated fragment can
survive.

## Candidate comparison

Measured in this spike (2026-08-16, Linux, ChatGPT plan: Free):

| Property | Chromium/Chrome + CDP | Firefox/Juggler | Camoufox |
|---|---|---|---|
| Runtime ownership | proven here (spawn, DevToolsActivePort, Target domain) | not tested | not tested |
| Transport-level network events | proven (Network domain) | equivalent domain exists (Juggler) | equivalent, not tested |
| Login gate (Cloudflare/Auth0) | clean launch passes; debugging-flagged launch challenged | unknown | designed to pass; unmeasured |
| Automation concealment | none used | n/a | stealth-oriented (measure per #16/#49) |
| Dependency footprint | single browser binary (~431 MB) | single browser binary | patched build |

Per the task: concealment/fingerprint controls are transport properties to
measure, not a reason to pick a winner by stealth. **No winner is declared
here.** The ADR must weigh: plain CDP is proven for the runtime but needs a
clean-launch enrollment; Camoufox/Firefox may reduce login friction but add
stealth-maintenance cost and are unmeasured in this repo.

## Portability/packaging

- Binary: `/usr/bin/google-chrome-stable` -> `/opt/google/chrome/chrome`
  (Chrome 151.0.7922.71), discovered via PATH `which` (or
  `RUNSTEAD_SPIKE_CHROME`); ~431 MB installed.
- Profile: dedicated dir owned by the spike, `DevToolsActivePort` used for
  port discovery (no fixed-port races).
- Process: spawned by the harness with a dedicated user-data-dir; stale
  owners of the same profile are terminated first.
- Requires a preinstalled Chrome/Chromium; packaging the browser later is
  plausible (Chrome for Testing bundles), not built here.
- Linux: tested (Linux Mint 22.3, X11). macOS/Windows: expected to work for
  the CDP flow (same flags/endpoints), but DevToolsActivePort path and
  process-exit semantics differ; not claimed as tested.

## Review-driven hardening (2026-08-17, zero additional model turns)

PR review #4953346768 requested changes before merge. Each point was
addressed without consuming any additional live model turn:

1. **Evidence consistency.** `evidence/live-key-events.json` no longer
   carries the stale v1 derived fields (`physical_sends: 0`,
   `response_started: false`, `completed_before_cancel`, the reused turn-1
   request id). `lib/rebuild_evidence.mjs` (v3) rebuilds every derived
   per-turn verdict from the corrected transport records + preserved DOM
   facts, and the three canonical artifacts
   (`network-turns-live.json`, `live-key-events.json`, `summary-live.json`)
   now agree with each other and with this document (asserted in the
   rebuild script).
2. **Cancellation provenance.** The live run was executed by harness v1 with
   sent-confirmed semantics; the current harness identifies versions exactly
   (v1/v2/v3) and its turn-2 code now implements the same semantics:
   `waitForSent` (conversation POST `requestWillBeSent`), stop click
   immediately, then `loadingFailed` observation. It never waits for response
   data before cancelling, so it reproduces the documented
   `response_started: false` abort. Provenance is recorded in every artifact.
3. **Conservative accounting.** The classifier now treats ANY unknown POST
   under `/backend-api/conversation/*` or `/backend-api/f/conversation/*` as
   `potential_model_effect` (uncertain) instead of silent `conversation_api_aux`;
   `potential_model_effect` blocks the "no hidden retry/fan-out" verdict
   (fixtures in `evidence/fail-closed-proofs.json`).
4. **Exact-origin trust boundary.** The DOM contract uses
   `location.origin === 'https://chatgpt.com'` (never a textual prefix) and
   an explicit auth-origin enumeration. Deterministic negative fixtures cover
   `https://chatgpt.com.evil.example`, `www.`/`sub.` subdomains, insecure
   scheme, and auth lookalikes.
5. **Redaction minimization.** Per-request evidence drops `other` traffic
   (aggregate counts preserved); conversation ids are replaced by
   non-correlatable placeholders (`conv#turn1`, `conv#turn2`) — including
   ids embedded in API paths (`<conv>`); `Target.targetCreated` never logs a
   raw URL (host + coarse path class only); query strings/fragments are
   always dropped.
6. **Timeout criterion covered.** Issue #16's "prove cancellation/timeout
   behavior" is now covered: a deterministic zero-turn state machine proves
   timeout -> `sent_timeout_fail_closed` -> no dispatch/replay, and the
   runtime wraps every bounded wait in a typed fail-closed path
   (`uncertain_timeout`, `re_dispatched: false`). Matrix row 15, PROVEN
   (synthetic). The matrix is now 19/19 PROVEN.

## Evidence

- `experiments/first-party-chatgpt-web-standalone/output/summary-live.json`
  — canonical live verdicts (rebuilt with the fixed/ conservative
  classifier; conversation ids as non-correlatable placeholders);
- `output/lifecycle-dry.json`, `output/summary-dry.json` — dry run
  (fixtures, pre-dispatch N=0, crash, fail-closed proofs), re-produced by
  the v3 harness on 2026-08-17 (0 model turns); `other` request streams are
  excluded from the persisted lifecycle (counts kept in the wrapper);
- `evidence/network-turns-live.json` — per-request transport records of the
  live run (accounting-relevant classes only; `other` dropped with aggregate
  counts preserved; conversation-id path segments replaced by `<conv>`);
- `evidence/network-turns.json` — per-request records of the v3 dry run
  (accounting-relevant classes only);
- `evidence/live-key-events.json` — canonical key events: per-turn derived
  verdicts rebuilt from transport records, request ids corrected, stale v1
  fields discarded, conversation ids as placeholders;
- `evidence/fail-closed-proofs.json` — deterministic zero-turn proofs
  (exact origin incl. lookalike negatives, conservative classifier, URL/
  target/conversation-id redaction, timeout fail-closed state machine),
  regenerated by `./test.sh`; 
- `test.sh` — reusable deterministic test entry point (proofs, classifier +
  path edge cases, redaction shaping, rebuild idempotence, cross-artifact
  agreement, derived-artifact leak scan); 0 model turns;
- `evidence/environment.json` — runtime/process/dependency proof (v3 dry
  re-validation);
- `evidence/auth-custody.json` — credential-handling proof;
- `evidence/login-enrollment.json` — enrollment flow + measured login-gate
  property.

**Harness versions (identified exactly, per review).**
- **v1** executed the live run on 2026-08-16 (budget 2/2) with the
  sent-confirmed cancellation flow: dispatch -> request in flight -> stop
  clicked before response start. Its redactor truncated classification
  strings and its turn windows were not per-turn scoped, so some in-memory
  totals and raw key-event verdicts were wrong (the raw sanitized transport
  records themselves were correct).
- **v2** fixed the classifier + per-turn scoping and re-derived the
  transport records from the preserved run log.
- **v3** (2026-08-17, review hardening, zero additional model turns):
  canonicalizes the whole evidence package from the corrected transport
  records + preserved DOM facts (`lib/rebuild_evidence.mjs`), applies the
  exact-origin contract, the conservative conversation-namespace
  classification, conversation-id placeholders, target URL shaping and the
  deterministic fail-closed/timeout proofs; the current `run_spike.mjs`
  cancellation code is aligned to the sent-confirmed semantics that v1
  actually executed. v3 was validated in dry mode (2026-08-17).

## Proven / unproven matrix

All 19 criteria: **19 PROVEN / 0 UNPROVEN / 0 FAILED** (with the caveats
stated in the result column; criterion 19 is proven by deterministic
zero-turn synthetic fixtures, criteria 1-18 by the live/dry runs as before).

| # | Criterion | Result |
|---|---|---|
| 1 | runs without Orca | PROVEN |
| 2 | runs without JCode | PROVEN |
| 3 | browser process launched by the spike | PROVEN |
| 4 | dedicated profile created/reused by the spike | PROVEN |
| 5 | auth remains in the profile | PROVEN |
| 6 | zero raw credentials in the harness | PROVEN (self-audit + allowlists) |
| 7 | CDP connection controlled directly | PROVEN |
| 8 | transport-level network observation | PROVEN (Network domain) |
| 9 | physical model-effect send count observable | PROVEN (1+1, N=0 pre) |
| 10 | one live text turn completes | PROVEN (token + terminal) |
| 11 | final response correlated to current turn | PROVEN |
| 12 | no hidden retry/fan-out observed | PROVEN (this run, conservative: any unknown conversation-namespace POST flips this to blocked) |
| 13 | pre-dispatch cancellation N=0 | PROVEN |
| 14 | post-dispatch cancellation classified | PROVEN (sent-confirmed -> aborted pre-response) |
| 15 | timeout behavior fails closed | PROVEN (deterministic zero-turn state machine + typed runtime waits; no replay) |
| 16 | browser/CDP crash classified | PROVEN (SIGKILL -> 1006, no replay) |
| 17 | unknown DOM state fail-closed | PROVEN (fixture + verdict matrix + exact-origin fixtures) |
| 18 | browser has no retry/policy/task truth authority | PROVEN by construction |
| 19 | no production dependency added | PROVEN (no go.mod/package changes) |

## Limitations

- Post-dispatch cancellation was observed only for an abort that landed
  before the first SSE byte (61 ms after dispatch). A mid-stream
  cancellation remains unmeasured (PR #72 could not observe any abort).
- First login requires a clean (flag-free) launch in this environment; a
  debugging-flagged launch is challenged by Cloudflare/Auth0. Runtime CDP
  operation with an enrolled session is unaffected.
- Single environment (Linux, Chrome 151, same authenticated ChatGPT account
  used by PR #72), N=1 per turn; ChatGPT Web DOM/endpoints change over time
  and the contract probes are version-sensitive.
- The live evidence was canonicalized after review (v3) from the preserved
  transport records + DOM facts; the raw facts are preserved in the
  canonical artifacts and the harness now produces correct artifacts
  directly. A fresh clean v2/v3 live run would consume 2 more model turns;
  not done.
- Timeout behavior is proven by deterministic zero-turn fixtures and the
  typed runtime wait path, not by an observed live timeout (an observed live
  timeout would require consuming another model turn and is not authorized).
- Firefox/Juggler and Camoufox remain unmeasured.

## ADR implications

- A standalone Chromium/CDP substrate is viable: all ownership
  responsibilities, transport-level accounting, cancellation and crash
  handling are proven without Orca/JCode.
- The Network domain is a credible authoritative source for physical
  model-effect send counting (browser-level, no page wrappers).
- Enrollment (login) and runtime have different launch requirements in the
  measured environment; the ADR should treat first-login as a distinct,
  user-assisted phase with a clean launch.
- Fail-closed page contract + typed crash/cancel states map cleanly onto
  Runstead's accounting vocabulary (`sent`, `response_started`, `aborted`,
  `uncertain`).
- The login-gate challenge is a measured transport property; concealment
  options (Camoufox) should be evaluated on measurements (#16/#49), not
  taste.
- This does not decide the ADR; it provides the substrate evidence the ADR
  needs.

## Recommendation

**GO** (for the ADR / next research step only; NOT production-ready).

The standalone first-party substrate question is answered: Runstead can
own/control a Chromium/CDP substrate directly for authenticated ChatGPT Web
runs, with transport-level accounting, cancellation and crash handling, and
zero dependency on Orca/JCode/OmniRoute. Remaining research (not started
here): mid-stream cancellation measurement, Firefox/Juggler and Camoufox
measurement, enrollment automation policy, and the eventual provider design.

---

## Provenance

**Source/project:** Chrome DevTools Protocol (Chrome 151.0.7922.71),
Node.js v24.19.0 built-ins; concepts from PR #72's Orca-based spike
(`experiments/first-party-chatgpt-web/`) and from JCode/Orca reverse
engineering notes in this repo.

**Observed concept:** a browser substrate fully owned by the caller —
spawned browser with dedicated profile, `DevToolsActivePort` port discovery,
Target-domain lifecycle, Network-domain transport accounting, DOM fail-closed
contract, DOM/Input interaction, stop-button cancellation, typed crash
handling — plus a measured login gate that challenges debugging-flagged
launches while clean launches pass.

**Problem solved:** issue #16's open question — whether a standalone
first-party ChatGPT Web substrate exists without another agent runtime.

**What Runstead may adopt:** Chromium/CDP as the standalone substrate
candidate; Network domain as authoritative send counter; DevToolsActivePort
discovery; profile-owned auth with clean-launch enrollment; fail-closed
contract; typed cancel/crash states.

**What is rejected:** dependence on Orca/JCode for the browser boundary;
fetch/XHR wrappers as authoritative accounting; hidden retries; treating
stealth tooling as a default rather than a measured option.

**Why:** the spike proves the substrate is viable and observable without
another runtime, and the measured login-gate property keeps the decision
honest (no concealment was silently added).

**Invariants:** governor remains the sole accounting boundary in production;
accounting stays conservative and fail-closed; task truth stays in SQLite;
policy/verifier remain in Go; no production changes; no credential
material; no more than 2 live model turns.

**Evidence gate:** this document plus the sanitized artifacts under
`experiments/first-party-chatgpt-web-standalone/` (`output/`, `evidence/`);
raw facts traceable to run logs preserved in this session.
