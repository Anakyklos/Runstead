# First-party ChatGPT Web spike (experiment, disposable)

Disposable live-proof prototype for issue #16 research (Refs #16). This
experiment is **not** the future `provider/chatgptweb`. It does not touch
production code, does not modify `go.mod`, and adds no production dependency.

## What it proves (results of the 2026-08-16 run)

- One bounded text turn through the Orca embedded browser (dedicated
  authenticated profile) produced the exact expected token
  `RUNSTEAD_FIRST_PARTY_OK`.
- Physical model-effect sends: **1** per turn (`POST /backend-api/f/conversation`),
  counted by a sanitized page-level fetch/XHR wrapper and cross-checked with
  browser resource timing. No hidden retry/fan-out observed.
- Cancellation before dispatch: **0** model-effect sends.
- Cancellation after dispatch: attempted with the second (final) live turn;
  the stop button was observed but the response completed before the click
  landed. UNPROVEN; see the report.
- Failure/drift: fail-closed classification matrix (5/5) plus live fixtures
  (`session_unavailable`, `contract_missing`, `login_required`).

Full report: `docs/research/first-party-chatgpt-web-spike.md`.

## Layout

- `instrument.js` — sanitized network observability injected via `orca eval`
- `harness.js` — fail-closed contract checks, composer fingerprinting,
  turn-state polling
- `run_spike.py` — orchestrator driving the `orca` CLI only
- `fixtures/logged-out.html` — login-marker fixture for fail-closed tests
- `output/` — sanitized lifecycle/summary artifacts of the run
- `evidence/` — sanitized resource timing + page-memory wrapper log

## How to re-run

Requirements: the user's Orca runtime running with the embedded browser, an
authenticated ChatGPT tab (any plan), Python 3 stdlib, `orca` CLI on PATH.
Budget: the live run consumes 2 model turns. A `--skip-live` mode runs the
classification matrix and fixtures only (0 model turns).

```sh
# dry validation (no model turns)
python3 run_spike.py --skip-live

# full live run (2 model turns)
python3 run_spike.py --tab <chatgpt-page-id>   # page id from: orca tab list
```

Notes:

- The prompt is bounded and text-only; no tools, images, files or browsing.
- The orchestrator never collects credentials: auth stays in the browser
  profile; the harness holds page ids and a11y refs only.
- Artifacts are sanitized: no headers, no cookies, no tokens, no
  Authorization material, no prompt/response bodies (only the bounded tokens
  and conversation ids used for correlation).
- `orca type`/`orca inserttext` did not reach the composer in this
  environment; the a11y `orca fill` path is used and verified by fingerprint
  before any dispatch.

## Provenance

- Source project/tool: JCode (`chatgpt_web.rs` in the local source snapshot),
  Orca embedded browser (`orca` CLI), ChatGPT Web.
- Observed concept: bounded browser boundary with auth in the profile, handle
  only in the core, fail-closed DOM contract, fingerprint-verified prompt
  entry, turn-scoped send counting, DOM terminal markers.
- Runstead problem addressed: #16 research on the smallest first-party
  ChatGPT Web transport that preserves the trust model and counts physical
  sends.
- Adopt: browser-boundary architecture concepts. Reject: JCode's Firefox
  extension dependency, N=1-by-construction accounting, hidden API retries as
  a pattern, Camoufox/fingerprint mimicry.
- Invariants preserved: governor sole accounting boundary, conservative
  fail-closed accounting, delivery-state replay unit, task truth in SQLite,
  policy/verifier in Go, no production changes.
