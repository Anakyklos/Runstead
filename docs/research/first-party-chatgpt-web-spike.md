# First-party ChatGPT Web transport spike

**Date:** 2026-08-16
**Status:** Research spike, disposable prototype. No production integration.
**Issue:** Refs #16 (research only; issue not closed)
**Branch:** `research/issue-16-first-party-spike`
**Base:** `30fec3067cb35840e72334830531e4c8f023fd21` (main at spike start)
**Live model turn budget used:** 2 of 2 (one success turn, one cancellation-after-dispatch attempt that completed)

---

## Motivation

The last live acceptance of #7 failed inside OmniRoute before any tool action:
Runstead received a malformed receipt, performed 1 admission / 0 authoritative
receipts / 1 conservative debit, marked the task `uncertain_reached` and did
not repeat. Post-hoc analysis found OmniRoute handoff/resume paths able to
generate additional POSTs outside the current accounting boundary. The
maintainer froze new investment in OmniRoute while this spike runs.

This spike answers, with evidence: *can Runstead perform a first-party ChatGPT
Web attempt through a dedicated authenticated session, deterministically
observing dispatch, the number of model-effect requests, the final response,
cancellation and failures, without giving the browser authority over
governor, retry, policy, task state or verifier?*

This is a disposable spike. It does not implement `provider/chatgptweb`, does
not change `runstead run`, does not touch OmniRoute, and does not modify any
production package.

## Scope and non-goals

In scope:

- investigate how JCode/Orca performs comparable authenticated access (local
  source, docs, runtime observation);
- one live text-only proof turn with bounded prompt
  (`Reply with exactly: RUNSTEAD_FIRST_PARTY_OK`) and sanitized lifecycle
  recording;
- physical model-effect request counting for that turn;
- cancellation before dispatch (zero model-effect requests);
- one additional live turn only to attempt cancellation after dispatch
  (budget 2/2, no automatic retry);
- failure/drift classification by fixture and pre-dispatch operations,
  fail-closed;
- candidate comparison and a proposed minimum boundary;
- this document plus disposable artifacts under
  `experiments/first-party-chatgpt-web/`.

Non-goals (explicitly forbidden by the task):

- no production `provider/chatgptweb`;
- no CLI changes;
- no OmniRoute removal or fixes;
- no `Closes #16`, no `Closes #29`, no `Closes #7`;
- no #17, #41, #57 work;
- no roadmap change;
- no general-purpose browser agent or router;
- no account rotation, fallback, hidden retries;
- no automated login/CAPTCHA/MFA;
- no more than 2 live model turns;
- no @Graph Mode;
- no credential collection, export, or persistence in the repo.

## Current Runstead invariants

From `docs/architecture.md`, `docs/account-protection.md` and the maintainer
addendum (`docs/research/jcode-reverse-engineering-maintainer-addendum.md`),
the invariants a first-party connector must preserve:

- the **governor** is the sole admission and accounting boundary for protected
  upstream account attempts: serialize per account, admit/delay/reject, account
  every authoritative upstream attempt, enforce pacing/rolling/task budgets
  and circuit state;
- local effects are separately controlled by protocol, policy, tools,
  executor and verifier boundaries;
- the provider seam stays narrow (a fake-provider-testable interface), not a
  generic routing framework;
- accounting is conservative: missing or structurally invalid accounting
  produces an uncertain debit and fails closed; observed amplification is fully
  accounted and then marks the lane unsafe;
- delivery evidence (`not_sent` / `sent_confirmed` / `sent_unconfirmed` /
  `response_started` / `completed`) is the transport-evidence unit for replay
  safety; it does not create an upstream idempotency guarantee;
- a first-party connector must not reproduce OmniRoute's rejected practices:
  TLS impersonation, fingerprint mimicry, silent account fallback, encryption
  fail-open, hard-coded frontend build values without drift probes;
- task truth stays in SQLite; policy/approval and verifier stay in Go.

## JCode/Orca mechanism observed

Sources: local JCode source tree at `~/.jcode/source/jcode`
(HEAD `aadd92f6d`, described `v0.67.1-43`; the running binary is v0.76.0, so
the source snapshot may lag the binary), JCode docs, and direct runtime
observation of the user's Orca. Prior static research exists in
`docs/research/jcode-reverse-engineering.md` (v0.70.1).

Answers to the investigation questions, with evidence:

1. **Does JCode drive a real browser?** Yes. The ChatGPT Web provider
   (`crates/jcode-provider-openai-runtime/src/chatgpt_web.rs`, 915 lines)
   drives a real Firefox through the Browser Agent Bridge
   (`~/.jcode/browser/browser-agent-bridge.xpi` +
   `firefox-agent-bridge-host`, native messaging manifest under
   `~/.mozilla/native-messaging-hosts/`). Setup/status logic is in
   `crates/jcode-base/src/browser.rs`.
2. **Automation protocol?** Extension + native messaging host. The Rust side
   shells out to the bridge CLI (`bridge_command(action, params)`), with
   actions `getActiveTab`, `fork`, `navigate`, `waitFor`, `fillForm`, `type`,
   `evaluate`, `click`, `getContent`, `screenshot`, `killFork`. The Orca
   embedded browser in this environment is **Chrome-based** (`orca tab profile
   list` → `source:chrome`), driven through the `orca` CLI (tab/eval/click/
   fill/snapshot); the CLI talks to the Orca runtime over a local unix socket
   (`~/.config/orca/o-*.sock`, `srw-------`).
3. **Bridge/sidecar local?** Yes: local native host + extension for JCode;
   local Orca runtime + CLI for this environment. No remote sidecar used.
4. **Private web transport called directly?** For the ChatGPT Web provider,
   JCode does **not** call a private API: it drives the real
   `chatgpt.com/?...&temporary-chat=true` page in the real browser. For its
   API providers it uses public APIs. Runstead's spike did the same: real page,
   real browser.
5. **Where does the authenticated session live?** In the browser profile.
   JCode requires the user to be logged in at chatgpt.com in Firefox and
   detects it via DOM. Orca keeps the Chrome profile at
   `~/.config/orca/profiles/local-default` (Cookies, Local Storage live
   there). The spike reused the already-authenticated ChatGPT session in the
   Orca embedded browser (user account redacted, Free plan).
6. **Does the runtime receive cookies/tokens or only a handle?** Only a
   handle. JCode passes `tabId`; the extension performs the actions in-page.
   The spike used Orca page ids and a11y element refs only; no cookie, token
   or header ever crossed the harness boundary.
7. **Final-response detection?** JCode polls the DOM every 750 ms: last
   `section[data-turn="assistant"]`, stop-button absence, terminal markers
   (`copy-turn-action-button` / Copy / feedback buttons), 8 stable polls,
   model-slug verification, alert detection. The spike used the same signal
   family (terminal markers + busy flag + expected-token hash match).
8. **Login expiry/challenge detection?** DOM-level pre-dispatch: JCode's
   `prepare_chatgpt_page` detects "Log in"/"Sign up" markers and bails; a
   workspace-onboarding dialog is never auto-confirmed. No mid-turn auth
   detection. The spike's harness classifies `login_required`,
   `contract_missing`, `dialog_blocking` and fails closed.
9. **Cancellation?** JCode: if the response consumer is closed, it clicks the
   stop button and bails (uncertain). The spike proved pre-dispatch
   cancellation (0 sends); post-dispatch cancellation was attempted and is
   documented as unproven (see Cancellation).
10. **Can one logical request produce multiple model-effect requests?** In
    JCode's ChatGPT Web path: one submit click, no retry loop, N=1 by
    construction but **not verified by network observation**. In JCode's API
    paths: yes (`openai_provider_impl.rs`: `MAX_RETRIES = 3`, `Retrying`
    phase, `RetryRollback`, persistent-WebSocket continuation). The spike's
    whole point is to observe N, not assume it: **N=1 observed per turn**.
11. **Hidden retry/replay/handoff?** In the web path, none in code and none
    observed. In API paths, retries and WS continuation exist (with
    `RetryRollback` of partial output). The spike observed no hidden retry or
    fan-out: exactly one `POST /backend-api/f/conversation` per turn.
12. **Model restriction architectural or product?** Product/contract. JCode
    pins `CHATGPT_WEB_MODEL`, passes `?model=...` in the URL, verifies the
    composer pill and the `data-message-model-slug` of the answer. Nothing
    architectural prevents other models.

## Authentication/session custody

- The spike reused the user's existing authenticated ChatGPT session in the
  Orca embedded browser (Default profile). **No login flow was run, no
  password was requested, no cookie/token was read or exported.**
- The session remained in browser custody the entire time: profile
  `~/.config/orca/profiles/local-default`; the harness held only page ids and
  element refs.
- JCode's equivalent: the user must log in at chatgpt.com in Firefox; JCode
  never collects credentials for this provider, and it refuses to act when the
  page reports signed-out state.
- Runstead's future connector can therefore satisfy "zero password
  collection" and "no raw credential in the core": the browser/profile owns
  auth; the core owns a handle.

## One-turn lifecycle

Observed successful turn (turn 1, conversation `6a810833…`):

1. harness contract check: `ready` (on chatgpt.com, composer present,
   signed-out markers absent, no dialog);
2. composer filled through the Orca accessibility path (`orca fill` on the
   a11y ref of the composer textbox; `orca type` and `orca inserttext` were
   tried and did not reach the page in this environment);
3. composer content verified by UTF-16 length + FNV-1a hash (43 / 0x776c1528)
   before any dispatch;
4. instrumentation phase set to `submitted`, send button clicked
   (`#composer-submit-button`, `data-testid="send-button"`);
5. DOM poll: stop button appeared (`response_started`), assistant text
   appeared (`response_streaming`), terminal markers appeared
   (`final_response`), text exactly matched
   `RUNSTEAD_FIRST_PARTY_OK` (hash-verified);
6. request log read and classified; artifacts written.

Turn 2 (cancellation attempt) followed the same lifecycle in the same
conversation and completed normally (see Cancellation).

## Physical request accounting

Main gate of the spike.

**Turn 1 (success):** `logical turns = 1`, `physical model-effect sends = 1`.
Observed via (a) a page-level `fetch`/XHR wrapper installed before dispatch
(`experiments/first-party-chatgpt-web/instrument.js`) and (b) browser
resource timing, cross-checked. The single model-effect request:

- `POST https://chatgpt.com/backend-api/f/conversation`, status 200,
  dispatched at 2026-08-16T00:45:38Z, phase `submitted`, no continuation or
  retry POST for that conversation.

23 other observed requests in the same window were classified auxiliary:
`sentinel/chat-requirements/prepare|ping|finalize` (challenge pre/post
flight), `ces/v1/t|p|rgstr|statsc` telemetry, `conversations` list,
`conversation/{id}/stream_status`, `conversation/{id}/textdocs`,
`f/conversation/prepare`, `lat/r`, `apps/sources_dropdown`.

**Turn 2 (cancel attempt):** `logical turns = 1`, `physical model-effect
sends = 1` (same endpoint, status 200, dispatched 2026-08-16T00:46:31Z,
completion ~3 s later). The wrapper's `dispatchedAt` proves the conversation
POST was the **first** dispatch of the turn (00:46:31.061), with aux
telemetry/sentinel dispatches following.

**Cancel-before-dispatch:** `logical turns = 0`, `physical model-effect
sends = 0` (only aux: `sentinel/ping` ×2, `f/conversation/prepare` GET,
`ces/statsc/flush`).

**Background activity:** the browser's resource timeline shows one unrelated
`POST /backend-api/f/conversation` at 2026-08-16T00:32:20Z, before any
instrumented window (page loaded at 00:15:37Z; likely user activity in their
own tab). The wrapper (installed per turn with `reset()`) never saw it. This
is exactly why per-turn scoped accounting windows are required, and why the
spike counted only within instrumented windows.

**Counting method and its boundary:** the count comes from page-context
instrumentation (wrapped `fetch`/XHR) plus resource timing, not from a
transport-level observer. Strengths: sees the exact backend POSTs including
method/status/timing; weaknesses: it depends on the page context (requests
from service workers or other isolated contexts would not be wrapped), and
the conversation-body clone adds latency. A CDP-level `Network` observer
would be strictly stronger evidence (see Candidate comparison). For this
spike, two independent page-level methods agreed on N=1.

**Classification rule used:** model-effect = `POST` to `chatgpt.com` whose
path matches `conversation` (excluding `/prepare` aux). Everything else is
auxiliary. `N > 1` would have been reported as-is; it was not observed.

## Final-response detection

The prototype knew the response was the current turn's final one through a
combination, in order of reliability:

1. **Turn identity:** the URL transitioned to `/c/<conversation-id>`; the
   in-turn `GET /backend-api/conversation/<id>/stream_status` and
   `.../textdocs` requests referenced the same id, and the only conversation
   POST in the window created it. (The wrapper's direct
   `conversation_id` extraction from the response body failed in this run —
   first-line-only parser; the instrument was fixed to scan all NDJSON lines
   for future runs. Correlation therefore used URL + in-turn GETs, which
   matched.)
2. **Response start:** the stop button appeared (`busy`), then assistant text
   appeared.
3. **Response end:** terminal markers (copy/feedback buttons) appeared **and**
   `busy` was false.
4. **Content correctness:** the final text exactly matched the bounded
   expected token (`RUNSTEAD_FIRST_PARTY_OK`), hash-verified; stale text
   would not match.

The smallest reliable set observed: *URL conversation id + single
conversation POST in window + terminal markers with `busy=false` + expected
token match*. No provider message-id abstraction exists at the DOM level; the
conversation id is the correlation anchor.

## Cancellation

- **Before dispatch: PROVEN.** Prompt composed and verified, decision made to
  cancel, composer cleared, nothing submitted: **0 model-effect requests**
  observed. No page authority was involved; the cancellation was a harness
  decision with no effect.
- **After dispatch: ATTEMPTED, NOT PROVEN.** Turn 2 was submitted; the stop
  button was observed (`busy=true`) ~1 s after dispatch, but the generation
  completed (exact token `RUNSTEAD_CANCELLED_OK`, hash-verified) before the
  separate click round-trip landed (`clickStop` returned `clicked:false`).
  The turn completed normally; 1 model-effect send; no retry. The stop
  mechanism exists on the page, but for fast generations the detect-and-click
  must be **atomic in one eval** (the harness now exposes
  `stopIfGenerating()` for future runs; the spike budget was already spent,
  so this was not re-run).
- No automatic retry was ever performed after any cancellation.

## Failure/drift behavior

All failure evidence is pre-dispatch or fixture-based; no model calls were
spent on it.

- **Session/profile unavailable:** `orca eval` against a nonexistent page id
  fails with `rc=1` "Browser page ... was not found" → classified
  `session_unavailable`, harness refuses to interact (no handle).
- **Unexpected page / contract unknown:** a tab at `about:blank`
  (rendered `data:text/html,`) returns `contract_missing`; no interaction.
- **Login-expired markers:** a fixture page containing "Log in"/"Sign up"
  buttons is detected (`signedOut=true`) and fails closed
  (`contract_missing` on origin, `login_required` on chatgpt origin; both
  refuse interaction).
- **Unknown modal:** classification matrix covers `dialog_blocking`; unknown
  dialogs are never auto-confirmed (same rule as JCode's onboarding refusal).
- **DOM drift observed live:** the composer label is localized
  (`aria-label="Converse com o ChatGPT"` in pt-BR vs JCode's hard-coded
  English label), and the send button uses `data-testid="send-button"` +
  `id="composer-submit-button"` (not `composer-send-button`). The harness
  probes selector variants and fails closed when none match.
- **Classification matrix:** the pure `classify()` function (origin /
  signed-out / dialog / composer) was unit-tested over 5 probes; only the
  nominal probe returns `ready`; the other 4 fail closed. Conclusion is
  **fail-closed**: no click/type/dispatch on unknown state.

## Candidate comparison

Current, evidence-based comparison for the transport Runstead would own
(after this spike; ADR completion is out of scope).

| Axis | A. JCode/Orca boundary (observed) | B. Chromium + CDP, Go client | C. Firefox/Juggler or Camoufox |
|---|---|---|---|
| Code Runstead must own | Thin: bounded client over `orca` CLI JSON (or Orca runtime protocol), ~hundreds of lines Go | Medium: CDP client (e.g. chromedp) + target/session management, ~1-3k lines or a library | Small-Medium: Juggler protocol client, or extension+native-host like JCode's agent bridge |
| Persistent profile | Yes, proven: Orca Chrome profile (`~/.config/orca/profiles/local-default`), reused | Yes: dedicated `--user-data-dir` | Yes: Firefox profile |
| Auth custody | Browser profile; proven zero-credential boundary | Browser profile | Browser profile |
| Observability / physical-send visibility | Page-level instrumentation via eval (fetch wrapper + resource timing); no transport-level network events exposed by `orca` CLI; page-context dependent (workers not covered) | **Native CDP Network domain**: per-request `requestWillBeSent`/`responseReceived`/abort, all targets incl. service workers; strongest send-count evidence | Juggler has network events; JCode's agent-bridge path is DOM/evaluate only (that is why JCode never counts sends) |
| Cancellation | Pre-dispatch proven; post-dispatch via page stop button, atomic-eval pattern available but unproven in this spike | Strong: CDP `Fetch.failRequest`/abort + page stop; state classifiable | Same page stop-button mechanism; similar race risk |
| Crash recovery | Runtime/CLI error classification proven (`session_unavailable`); Orca runtime reconnect semantics not exercised | Well understood: WS close → target rediscovery, explicit reconnect | Extension/native-host restart; JCode restarts sessions |
| DOM/UI drift | High, observed (localized labels, testid changes); mitigated by selector variants + fail-closed matrix | Same app-level drift; CDP selectors no more stable | Same app-level drift |
| Dependencies | Orca CLI + Orca runtime already present in the user's environment; **zero new dependencies for the prototype** | Chromium binary (~150-300 MB) + Go CDP dependency | Firefox + extension + native host (JCode already ships this pattern) |
| Packaging | Requires Orca runtime presence; prototype = scripts + orca CLI | Single binary + bundled/assumed Chromium | JCode-style `browser/` dir + native manifest |
| Second language | None (Go ↔ orca JSON) | None | None |
| Maintenance | Orca protocol is third-party and evolving (draft-ish); small surface | CDP is stable and versioned; large but mature surface | Juggler stable; agent-bridge is jcode-owned but Firefox-specific |
| Risk of becoming a second control plane | LOW if kept a thin client; MEDIUM if the Orca runtime starts to look like a router | LOW |

**Orca-independence caveat (review blocker, mandatory):** the live proof ran
entirely on the user's Orca runtime/CLI/profile. That proves the viability of
a *browser boundary*, **not** that Runstead already has a self-sufficient
first-party provider. Without Orca this prototype does not run, and a
standalone Runstead-owned substrate (any of the three candidates above,
installed and exercised outside the Orca environment) remains **UNPROVEN**.
The boundary proposal below therefore does **not** privilege the
`orca CLI/runtime` path for production; it is one candidate whose production
claim requires a later gate proving installation, lifecycle, network
visibility, cancellation/crash behavior and profile custody outside the Orca
environment. This point stays research-only. LOW |

Sources for technical claims: JCode local source (cited above), the observed
`orca` CLI surface and runtime socket, and this spike's live observations.
No claim in this table prefers Rust, Chromium or Camoufox by taste. Per #16
and #49, fingerprint controls / automation concealment are **transport
properties to be measured, not categorically prohibited**; Camoufox therefore
remains a research candidate until evidence or the #16 ADR decides. The
OmniRoute audit's rejections (TLS impersonation, hidden retry/accounting
gaps) concern that implementation's accounting behavior, not fingerprint
behavior in general.

## Proposed minimum boundary

Conceptually, if Runstead pursues this path (no integration in this spike):

```text
Runstead Go (governor, retry, delivery/accounting, task truth SQLite,
            policy/approval, verifier, evidence)
   └─ provider/chatgptweb (thin Go adapter, future)
        └─ bounded session boundary (candidate transports, not yet chosen:
           thin client over orca CLI/runtime, or Chromium CDP client,
           or Firefox/Juggler-style protocol client)
             └─ dedicated authenticated browser profile (auth custody)
                  └─ ChatGPT Web (real page, real browser)
```

The transport is explicitly **not chosen** by this spike. The orca path is
the one that was exercised and works in the user's environment today, but
its production candidacy is gated on a standalone-substrate proof outside
Orca (installation, lifecycle, network visibility, cancellation/crash,
profile custody).

Rules demonstrated by the spike and required for the boundary:

- the browser never decides retry, fallback, account rotation, acceptance,
  task completion, policy, or governor admission;
- dispatch accounting is derived from observed physical sends (instrumented
  window), and is conservative on any unknown (`UNKNOWN` rather than assumed
  1);
- final response is accepted only on turn-scoped correlation (conversation
  id + windowed send count + terminal markers + expected content hash);
- every interaction is gated by the fail-closed contract matrix;
- prompt entry is verified by fingerprint before dispatch (no partial
  submits);
- no credential material, header, or body content crosses the boundary.

## Security/secret handling

- No password requested or collected; no cookie export; no token read; no
  localStorage/profile dump; no Authorization header logged; no prompt or
  response body stored except the two bounded spike tokens.
- The session stayed in the browser profile; the harness used page ids and
  element refs only.
- Artifacts are sanitized (method, hostname, path without query, status,
  timestamps). Conversation ids and page ids are truncated to their first 8
  hex chars; runtime ids, pids, account names, personal paths and unrelated
  tab listings are removed. Audited: no `Authorization`, `access_token`,
  `refresh_token`, password, or exact prompt/response text in the artifact
  files.
- The instrument captures only `conversation_id`/message id from backend
  bodies; everything else is discarded. The orchestrator never logged headers
  or bodies.
- Nothing was persisted to any Runstead SQLite database. Nothing was added to
  `go.mod`.

## Evidence collected

Artifacts (all committed under `experiments/first-party-chatgpt-web/`): Identifiers in artifacts are truncated/redacted (see Security/secret handling).

- `instrument.js` — sanitized network observability (fetch/XHR wrapper,
  conversation-id-only body extraction);
- `harness.js` — fail-closed contract matrix, composer fingerprinting,
  turn-state polling, send/stop/clear primitives;
- `run_spike.py` — disposable orchestrator driving `orca` CLI only;
- `fixtures/logged-out.html` — login-marker fixture;
- `output/lifecycle.json` — sanitized lifecycle record of every phase;
- `output/summary.json` — gate summary (success / cancel-pre / cancel-post);
- `evidence/performance-resource-timing.json` — sanitized resource timeline
  mapping every conversation POST to absolute time;
- `evidence/turn2-wrapper-log.json` — page-memory wrapper log for turn 2 with
  dispatch timestamps.

Key numbers:

- success turn: 1 logical turn, **1 physical model-effect send**
  (`POST /backend-api/f/conversation`, 200), 23 auxiliary requests, final
  text exact-match, conversation correlated by id;
- cancel-before-dispatch: **0 model-effect sends**;
- cancel-after-dispatch attempt: 1 send, response completed, stop click did
  not land (unproven);
- hidden retry/fan-out: none observed in either turn;
- failure fixtures: all fail closed (`session_unavailable`,
  `contract_missing`, `login_required`, `dialog_blocking` matrix 5/5).

## Limitations / unknowns

- **Post-dispatch cancellation is UNPROVEN.** The stop button appeared but
  the response completed before the separate click; the atomic
  detect-and-stop pattern was added for future runs but not re-run (budget
  2/2).
- **Send-count evidence is page-context instrumentation**, not transport
  level. Two independent page-level methods agreed (N=1), but service-worker
  traffic would be invisible to a page wrapper; a CDP `Network` observer is
  the stronger follow-up.
- **`conversation_id` body extraction failed on the first-line-only parser**
  in this run; correlation used URL + in-turn GETs instead. The instrument is
  fixed (scan all lines) but was not re-validated live.
- **One unrelated background conversation POST** exists on the page's
  resource timeline before the spike's windows (user/browser activity,
  00:32:20Z). It did not enter any instrumented window; it demonstrates why
  turn-scoped windows are mandatory.
- **Orca runtime dependency (standalone substrate UNPROVEN):** the
  prototype requires the user's Orca runtime and does not run without it.
  Reconnect/crash semantics of the Orca runtime itself were not exercised
  (only CLI-level failure classification). A standalone Runstead-owned
  substrate must be proven (installation, lifecycle, network visibility,
  cancellation/crash, profile custody) before any production claim.
- **The JCode source snapshot may lag the running binary** (source described
  v0.67.1-43, binary v0.76.0); the chatgpt-web provider facts were read from
  the snapshot and match observed bridge behavior.
- **Model/plan variability:** this ran on a Free-plan account with trivial
  prompts; sentinel/turn-continuation behavior may differ for longer turns or
  paid models. No claim about N for other conditions.

## Go/no-go recommendation

**GO apenas para a próxima pesquisa standalone** — not a go-live of any
provider. The spike is technically promising by its own criteria:

| Criterion | Result |
|---|---|
| 1 dedicated reusable authenticated session | PROVEN |
| 2 zero password collection | PROVEN |
| 3 no raw credential in core | PROVEN |
| 4 real text turn produced | PROVEN (2 turns, exact tokens) |
| 5 final response correlated to current turn | PROVEN (conversation id + windowed send + terminal markers + hash) |
| 6 physical send count observable | PROVEN for this environment (N=1, two independent methods); transport-level (CDP) counting is the stronger follow-up |
| 7 no hidden retry/fan-out in success case | PROVEN for observed turns (1 POST each) |
| 8 pre-dispatch cancellation without effect | PROVEN (0 sends) |
| 9 browser/session failure classifiable | PROVEN (fail-closed matrix + CLI errors) |
| 10 boundary smaller than a general browser agent/router | PROVEN (thin harness; no routing, no fallback) |
| 11 policy/task truth not delegated to browser | PROVEN (nothing but the prompt reached the page) |
| 12 standalone Runstead-owned substrate outside Orca | **UNPROVEN** (prototype requires the Orca runtime; production candidacy needs the standalone gate) |

Post-dispatch cancellation stays UNPROVEN and must be proven before any
production design depends on stop semantics. The orca path must not be
treated as a production default: without Orca this prototype does not run,
and the standalone substrate gate (installation, lifecycle, network
visibility, cancellation/crash, profile custody outside Orca) is mandatory
before any design proceeds. Nothing in this spike justifies closing #16; the
ADR and the bake-off remain the maintainer's decisions.

## Next research required

- **standalone-substrate gate (mandatory before any production claim):**
  prove installation, lifecycle, network visibility, cancellation/crash and
  profile custody for a Runstead-owned substrate outside the Orca
  environment; without it the orca path stays research-only;
- prove post-dispatch cancellation with the atomic `stopIfGenerating`
  pattern (1 more live turn, separate budget);
- obtain transport-level send visibility (CDP `Network` observer or
  equivalent) and re-confirm N=1 on a longer, multi-chunk response;
- re-validate conversation-id extraction from the response body (fixed
  instrument);
- measure fingerprint/automation-concealment properties as transport
  properties per #16/#49 (Camoufox stays a research candidate; the #16 ADR
  decides);
- decide the boundary mechanism (thin orca CLI client vs Chromium CDP vs
  Firefox/Juggler/Camoufox) as the #16 ADR, using the comparison table above;
- confirm Orca runtime crash/reconnect behavior with the user's runtime
  before relying on it.

---

## Provenance

- **Source project/tool:** JCode (open source, local snapshot
  `aadd92f6d`), its ChatGPT Web provider and Browser Agent Bridge; the user's
  Orca embedded browser (Chrome-based runtime + `orca` CLI); OpenAI ChatGPT
  Web (public web app).
- **Observed concept:** a bounded browser boundary: real page + real browser,
  persistent profile holding auth, core holding only tab/page handles, DOM
  contract checks that fail closed, prompt fingerprint verification before
  dispatch, DOM terminal markers for final response, stop-button cancellation,
  per-turn sanitized request observability.
- **Runstead problem addressed:** #16 research: smallest way to access
  ChatGPT Web first-party without OmniRoute while keeping governor, retry,
  delivery/accounting, task truth, policy and verifier in Go, and while
  counting physical model-effect sends instead of assuming 1.
- **What we could adopt:** the browser-boundary architecture (dedicated
  profile = auth custody; handle-only core; fail-closed contract matrix;
  fingerprint-verified prompt entry; turn-scoped send counting;
  DOM-terminal detection; conservative uncertain handling).
- **What we reject:** JCode's Firefox-extension dependency for Runstead
  (environment-specific), its lack of network-level send accounting, its
  N=1-by-construction assumption, and its API-path hidden retries/WS handoff
  as a pattern for the web path. Fingerprint/automation-concealment behavior
  (Camoufox) is **not** rejected here: per #16/#49 it is a transport property
  to be measured, and it stays a research candidate until the ADR decides.
- **Why:** the observed boundary is smaller than a browser agent/router,
  keeps auth under browser custody with zero credentials in the core, and
  makes the physical send count observable with the right instrumentation;
  the rejected items duplicate OmniRoute's accountability gaps (hidden
  retries, N assumed not counted).
- **Invariants preserved:** governor sole admission/accounting boundary;
  conservative fail-closed accounting; delivery-state evidence as the replay
  unit; no silent retry/fallback/rotation; task truth in SQLite; policy and
  verifier in Go; no production dependencies added; no production code
  touched.
- **Evidence gate:** success turn N=1 (two independent page-level methods);
  cancel-before-dispatch N=0; no hidden retry observed; final response
  correlated by conversation id + terminal markers + exact hash; failure
  fixtures fail closed (matrix 5/5). Post-dispatch cancellation:
  UNPROVEN.
