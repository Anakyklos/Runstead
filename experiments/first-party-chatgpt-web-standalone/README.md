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
  clicked 61 ms after dispatch, request aborted
  (`net::ERR_ABORTED`, `canceled: true`) before the first SSE chunk.
- **Pre-dispatch cancellation**: N=0 model-effect sends.
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
- `lib/sanitize.mjs` — structural redaction for all evidence
- `lib/rebuild_evidence.mjs` — one-off evidence rebuild for the v1 live run
  (see Evidence note below)
- `fixtures/logged-out.html` — wrong-origin fail-closed fixture
- `profiles/` — dedicated profile (gitignored; holds real auth session)
- `output/`, `evidence/` — sanitized artifacts of the runs

## How to re-run

Requirements: Google Chrome or Chromium on PATH (or
`RUNSTEAD_SPIKE_CHROME`), Node >= 22 (built-in WebSocket), no other runtime.

```sh
# first login (user-assisted, clean window, no flags): log in, close window
node run_spike.mjs login

# dry validation (0 model turns): fixtures, pre-dispatch cancel N=0, crash
node run_spike.mjs dry

# live proof (2 model turns): success turn + post-dispatch cancellation + crash
node run_spike.mjs live
```

Budget: exactly 2 live model turns. Do not exceed.

## Evidence note (harness v1 accounting bug)

The first live run was recorded by harness v1, which had two
accounting-label bugs (redactor mangled classification strings; turn
windows not scoped per turn). The raw sanitized records were preserved in
the run log. `lib/rebuild_evidence.mjs` re-derives classification with the
fixed classifier and attributes turns by the recorded dispatch timestamps.
The harness itself was fixed (v2) and re-validated in dry mode; future live
runs produce correct artifacts directly.

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
