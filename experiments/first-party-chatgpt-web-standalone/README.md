# Standalone first-party browser substrate spike (experiment, disposable)

Disposable live-proof prototype for issue #16 research (Refs #16): can
Runstead own/control a browser substrate for ChatGPT Web directly, with a
dedicated profile, transport-level network observability, cancellation and
crash handling, WITHOUT Orca runtime, orca CLI, JCode runtime, JCode Browser
Agent Bridge or OmniRoute?

This experiment is **not** the future `provider/chatgptweb`. It does not
touch production code, does not modify `go.mod`, and adds no dependency.

## What it proves (2026-08-16 run)

- **Standalone substrate**: Google Chrome 151 is launched by the harness with
  a dedicated profile; CDP endpoint discovered via `DevToolsActivePort`;
  target discovered/attached via the Target domain; all interaction via CDP
  (`Runtime.evaluate`, `Input.insertText`, DOM click). Zero Orca/JCode/OmniRoute.
- **Transport-level accounting**: physical model-effect sends counted from
  CDP Network events only (`POST /backend-api/f/conversation`). No
  fetch/XHR wrapper injected.
- **Turn 1 (success)**: 1 physical send, HTTP 200 `text/event-stream`,
  12017 bytes streamed, terminal DOM, `RUNSTEAD_STANDALONE_OK` present,
  conversation correlated, no hidden retry/fan-out.
- **Turn 2 (post-dispatch cancellation)**: 1 physical send, stop button
  clicked 61 ms after dispatch (sent-confirmed semantics: the harness waits
  for the conversation POST to be in flight, NOT for response data), request
  aborted (`net::ERR_ABORTED`, `canceled: true`) before the first SSE chunk.
- **Timeout**: deterministic zero-turn proof
  (`evidence/fail-closed-proofs.json`) that a sent-but-never-started turn
  derives `sent_timeout_fail_closed` with no dispatch/replay; every bounded
  wait in the harness is a typed fail-closed path (`uncertain_timeout`).
- **Pre-dispatch cancellation**: N=0 model-effect sends.
- **Exact-origin contract**: `location.origin === 'https://chatgpt.com'`
  (never a textual prefix); lookalikes like
  `https://chatgpt.com.evil.example` fail closed (deterministic fixtures).
- **Conservative accounting**: any unknown POST under
  `/backend-api/conversation/*` becomes `potential_model_effect` and blocks
  the no-hidden-retry verdict instead of passing as auxiliary.
- **Crash/disconnect**: SIGKILL of the browser -> CDP close code 1006,
  typed fail-closed state, zero automatic replay.
- **Measured login-gate property**: OpenAI's login gate (Cloudflare
  Turnstile + Auth0) challenges Chrome launches with remote-debugging flags;
  a clean (flag-free) launch logs in fine. Enrollment therefore uses a clean
  launch; the authenticated runtime uses CDP. No automation concealment used.

Full report: `docs/research/first-party-chatgpt-web-standalone-spike.md`.

## Layout

- `run_spike.mjs` — orchestrator (modes: `login`, `dry`, `live`)
- `lib/cdp.mjs` — minimal CDP client over Node's built-in WebSocket
- `lib/browser.mjs` — chrome discovery/launch/profile/port/targets
- `lib/network.mjs` — transport-level accounting (CDP Network events)
- `lib/dom.mjs` — fail-closed page contract, composer, turn state
- `lib/sanitize.mjs` — structural redaction for all evidence (incl. exact
  URL/target shaping and conversation-id placeholders)
- `lib/proofs.mjs` — deterministic zero-turn fail-closed proofs (origin,
  classifier, redaction, timeout)
- `lib/rebuild_evidence.mjs` — canonical rebuild of the live evidence
  (v3; see Evidence note below)
- `test.sh` — deterministic 0-model-turn test entry point (proofs,
  classifier + conversation-path edge cases, redaction shaping, rebuild
  idempotence, cross-artifact agreement, leak scan)
- `fixtures/logged-out.html` — wrong-origin fail-closed fixture
- `profiles/` — dedicated profile (gitignored; holds real auth session)
- `output/`, `evidence/` — sanitized artifacts of the runs (incl.
  `evidence/fail-closed-proofs.json`)

## How to re-run

Requirements: Google Chrome or Chromium on PATH (or
`RUNSTEAD_SPIKE_CHROME`), Node >= 22 (built-in WebSocket), no other runtime.

```sh
# deterministic suite (0 model turns): proofs, classifier/path edge cases,
# redaction shaping, rebuild idempotence, cross-artifact agreement, leak scan
./test.sh

# first login (user-assisted, clean window, no flags): log in, close window
node run_spike.mjs login

# dry validation (0 model turns): fixtures, pre-dispatch cancel N=0, crash
node run_spike.mjs dry

# live proof (2 model turns): success turn + post-dispatch cancellation + crash
node run_spike.mjs live
```

Budget: exactly 2 live model turns. Do not exceed.

## Evidence note (harness versions)

- **v1** executed the live run (2026-08-16, budget 2/2) with sent-confirmed
  cancellation semantics. Its redactor mangled classification strings and
  its turn windows were not per-turn scoped, so some in-memory totals and
  raw key-event verdicts were wrong (the raw sanitized transport records
  were correct).
- **v2** fixed the classifier + per-turn scoping and re-derived the
  transport records from the preserved run log.
- **v3** (2026-08-17, review hardening, zero additional model turns):
  `lib/rebuild_evidence.mjs` canonicalizes the whole evidence package from
  the corrected records + preserved DOM facts (no stale derived fields),
  applies exact-origin checks, conservative conversation-namespace
  classification, conversation-id placeholders, target URL shaping and
  deterministic fail-closed/timeout proofs. `run_spike.mjs` is aligned to
  the sent-confirmed cancellation semantics that v1 actually executed, and
  was re-validated in dry mode. Run `node lib/rebuild_evidence.mjs` to
  re-canonicalize; run `node run_spike.mjs dry` to re-run the proofs (0
  model turns).

## Provenance

- Source project/tool: Chrome DevTools Protocol (Chrome 151), Node built-ins.
- Observed concept: browser-owned transport observability (Network domain),
  profile-resident auth, fail-closed DOM contract, two-phase enrollment.
- Runstead problem addressed: #16 — can Runstead own the smallest ChatGPT
  Web substrate without another agent runtime.
- Adopt: CDP Network domain as authoritative send counter; DevToolsActivePort
  discovery; profile-owned auth; clean-launch enrollment. Reject: relying on
  another agent runtime; fetch/XHR wrappers as authoritative; hidden retries.
- Invariants preserved: governor sole accounting boundary in production,
  fail-closed accounting, no production changes, no credential capture.
