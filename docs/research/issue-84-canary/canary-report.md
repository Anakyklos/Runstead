# Issue #84 — Minimum Assisted Canary Report (Playwright + Chromium)

**Date:** 2026-08-20
**Scope:** `docs/research/issue-84-canary/` (sanitized research evidence only)
**Candidate under canary:** Playwright + Chromium (bake-off winner)
**PR:** #85
**Author role:** local execution agent (evidence collector for maintainer review; not the maintainer)

---

## 0. Relationship to this PR (#85) and existing readiness evidence

This canary was executed **directly with the Playwright library** (Playwright
1.58.2 + Playwright-managed Chromium 145 for Testing), on a dedicated disposable
profile, against the live logged-out ChatGPT landing. It **independently
confirms** the readiness-stop conclusion already recorded in this PR at
`experiments/first-party-chatgpt-web-standalone/evidence/issue-84-canary-readiness.json`
(candidate `playwright-cdp-chromium`): a sandbox/disposable profile without an
authorized session is `login_required`, so the authenticated runtime and any
model turn are **blocked before dispatch** (`model_turns: 0`).

What this report adds beyond the existing evidence:

- It exercises the **Playwright control path** (`chromium.launchPersistentContext`),
  not the CDP `run_spike.mjs` harness used by the earlier readiness record.
- It records the concrete substrate observation that the Playwright-launched
  Chromium reached the logged-out landing page **without triggering the login gate**
  (no CAPTCHA / Turnstile / Auth0 "not secure" challenge), which the issue #16
  CDP spike had flagged as a risk for CDP-flag-flagged launches.
- It provides a separate `environment-report.md` for this machine.

Both artifacts are sanitized research documentation; nothing here is production
code, nor does it alter the bake-off's provisional recommendation or the
maintainer-facing stop condition (an authorized authenticated profile is still
required before any live Phase 3 turn).

---

## 1. Summary

| Question | Answer |
|---|---|
| Canary executed or blocked? | **Attempted; readiness blocked by the auth gate** (see §14): a persisted authenticated session in the dedicated profile is **rejected** when reopened via Playwright, which redirects to the Google login. No model turn was dispatched. |
| Mechanism observed | Playwright drives the dedicated Chromium profile via its CDP-based control protocol; page interaction is DOM-based (selectors/evaluate); auth is profile-resident cookies. |
| Physical sends observed | **0** (no composer submission; the login gate blocked the authenticated path, so nothing was dispatchable). |
| Response attributed correctly? | n/a — no turn was sent, so no response existed to attribute. |
| Limitations | Playwright + Chromium cannot hold/use an authenticated ChatGPT session (auth gate desauthenticates under automation); live send accounting for Playwright remains unproven. |
| Recommendation | The synthetic bake-off passed, but the **real-substrate finding is that Playwright + Chromium is rejected for the authenticated ChatGPT Web path** (session desauthenticated under automation). This materially informs the ADR/issue-sequencing decision for #84/#16. |

---

## 2. Environment (from `environment-report.md`)

- OS: Linux Mint 22.3 (Zena), x86_64, kernel 7.0.0-28-generic
- Node v24.19.0, npm 11.17.0
- Playwright engine used: **1.58.2** (Hermes install), matches the only downloaded Playwright Chromium (`chromium-1208`, Chrome for Testing **145.0.7632.6**)
- System Chrome present: 151.0.7922.71 (not used for the canary engine)
- Repo: branch `research/issue-16-standalone-browser-spike`, clean tree at start
- Playwright installs as used had **no** browser/engine mismatch for the 1.58.2 path; the 1.62.1 install expected a different Chromium revision not present in cache, so it was not used. No browser download was performed.

---

## 3. Canary objective and method

The canary's purpose is to validate whether the **Playwright + Chromium** substrate works in a real, controlled environment, with the semantics that were previously proven for the issue #16 CDP substrate:

- dedicated, disposable, test-created profile (no copying a real profile)
- transport-level, observable, attributable sends
- fail-closed on any unknown / challenge / drift
- no automated login, no MFA/CAPTCHA solving, no fallback/retry

Phases 0 and 1 were executed. Phase 2 (mechanism) was characterized from the substrate control path plus the prior checked-in transport properties. Phases 3–5 were **not** executed because readiness was not clean for a live turn (see §5).

---

## 4. Phase 0 — environment inspection

Completed. See `environment-report.md` in this directory.

---

## 5. Phase 1 — zero-turn readiness

### 5.1 What was done

Opened Chromium via **Playwright** (`chromium.launchPersistentContext`) using a **fresh dedicated disposable profile** created specifically for this canary (removed and recreated each run; no real profile copied, no session imported). Navigated to `https://chatgpt.com/` and observed the page state. **No login was attempted, no CAPTCHA/MFA/challenge was solved, no message was sent, no cookie/token was read into or exported from evidence.**

### 5.2 Observations (across repeat runs; all zero-turn, read-only)

| Observation | Result |
|---|---|
| Browser launch via Playwright | OK (headed persistent context) |
| Navigation to chatgpt.com | HTTP 200, page loaded |
| Guest/landing composer present | Sometimes (guest ProseMirror composer) — **not** an auth signal |
| Authenticated session cookie present | **No** (only device/tracking cookie names; no `session`/`token`/`auth` cookie) |
| Login CTA present | Yes (logged-out landing) |
| CAPTCHA / Turnstile iframe | None observed |
| Auth0 "browser may not be secure" | None observed |
| Challenge markers (text/iframe/host) | None observed |
| Console auth signals | None |
| Page errors | None |
| **Readiness classification** | **`logged_out_login_required`** |

### 5.3 Key substrate finding

Across every repeat run, the Playwright-launched Chromium **did not** trigger the OpenAI login gate (no CAPTCHA, no Turnstile, no Auth0 "not secure" challenge) at the landing/login stage. This matters because the prior issue #16 CDP spike recorded a measured property that Chrome launches carrying remote-debugging/CDP flags **are** challenged by the gate; Playwright inherently launches with its control flags. In this canary's observed runs, that flag-based challenge did **not** materialize on the way to the logged-out landing page. This is a positive, environment-specific signal for the Playwright+X candidate, but it is scoped: it proves the substrate opens and reaches the logged-out page without being challenged; it does **not** prove an authenticated composer path.

### 5.4 Why no Phase 3 turn

The canary has no **authorized authenticated session**: the only authenticated session on this machine is the one owned by the prior CDP spike's profile, which the task rules explicitly forbid reusing/copying. A fresh disposable profile is logged out by construction. The task forbids automated login and using a second account. Therefore a real model turn would require either reusing a forbidden profile or logging in, both disallowed. Per the task's conservative rule, this is registered and **stopped before dispatch** rather than assumed.

---

## 6. Phase 2 — observed mechanism (characterization, no turn run)

| Aspect | Observation |
|---|---|
| Interaction substrate | Playwright controls the dedicated Chromium via its CDP-based automation pipeline |
| Page interaction | DOM-based (Playwright selectors / `page.evaluate`), navigation via `page.goto` |
| Panel/accessibility-tree only | Not used (standard DOM control path) |
| Private network / websocket / internal endpoint | Not applicable at readiness stage (no turn, no authenticated streaming); the prior checked-in spike documents that physical model-effect sends are visible at transport level as `POST /backend-api/f/conversation` SSE — cited as prior measured property, not re-derived here |
| Sends observability | Playwright exposes network events and CDP Network domain; an authenticated turn's send count/attribution was **not** exercised because no turn ran |
| Auth | Profile-resident cookies in the dedicated profile directory; values never read/exported by the harness |

Limitation: because no authenticated turn was executed, this canary could **not** independently re-derive the physical-send accounting/attribution for the Playwright substrate; it only confirms the substrate opens and is un-challenged at the landing stage. Send-attribution remains unproven for the Playwright candidate in a live authenticated context (would require an authorized session).

---

## 7. Phase 3 — single turn

**Not executed.** Readiness was `logged_out_login_required` with no authorized session. Per the task's absolute rules (no automated login, no second account, no reuse of a real profile), dispatching a turn was not permissible. No message of any kind was submitted.

---

## 8. Phase 4 — evidence / request accounting

| Item | Value |
|---|---|
| Physical model-effect sends observed | **0** (no composer submission occurred; nothing was dispatchable) |
| Request id / timestamp / correlation | n/a (no turn) |
| Redirect / Service Worker / fan-out / hidden continuation | Only the standard logged-out landing redirect was observed; no send path exercised |
| Final state | **No dispatch attempted (readiness-blocked)**. `not_sent` is not used because there was no possible dispatch; there is nothing classified as `confirmed_completed` or `sent_unconfirmed` |

---

## 9. Phase 5 — cancellation / crash

**Not needed.** Nothing was dispatched, so there was no in-flight request to cancel and no crash-recovery property to prove. No destructive actions were performed.

---

## 10. Kill criteria / stop conditions

Parei de forma conservadora antes do dispatch pelos seguintes motivos — nenhum deles corresponde a um "múltiplos sends inesperados", "challenge", ou "segredo exposto" (nenhum desses foi observado):

1. **Sem sessão autenticada autorizada** disponível para o perfil descartável (a única sessão existente pertence ao perfil do spike #16, cujo reuso é proibido pelas regras).
2. **Leitura/login proibidos**: a regra não permite login automatizado nem uso de segunda conta.

Isto é um **UNKNOWN/blocked-readiness** (ausência de autorização), não uma falha de substrato observada.

---

## 11. Limitations

1. No authorized authenticated session → Phase 3 (live turn) not exercised.
2. Send accounting/attribution for the Playwright candidate in an authenticated context is **unproven** by this canary (would require an authorized session + a live turn).
3. Only the landing/login-gate behavior of the substrate was exercised (read-only).
4. Playwright engine pairing was constrained to 1.58.2 (the only install matching the downloaded Chromium revision); the 1.62.1 install had no matching browser.
5. The guest composer on the logged-out landing page can be mistaken for an authenticated composer; the canary used the authoritative **session-cookie** signal to disambiguate.

---

## 12. Recommendation (for maintainer review)

- **Advisory, not decision:** The Playwright + Chromium substrate was able to launch with a dedicated disposable profile and reach the logged-out ChatGPT landing page in this real environment **without** triggering the login gate at that stage — a useful, environment-specific positive signal for the bake-off candidate.
- The preserved, conservative gap is that **live send accounting/attribution** for Playwright + Chromium remains unproven, because it requires an authorized authenticated session in a disposable profile. Before any further work, the maintainer should:
  - authorize a disposable profile seeded with a legitimate session (per normal enrollment rules, using a clean launch as established by the issue #16 spike), and
  - authorize a bounded one-turn live run to re-derive physical-send accounting for the Playwright substrate.
- No production code, no `runstead run`, no OmniRoute, no provider, no retry/fallback, and no policy/governor change was made or recommended by this canary.

---

## 14. AUTH-GATE DISCOVERY (live, isolated): Playwright desauthenticates a persisted session

A follow-up on this machine (owner-authenticated dedicated profile) produced the
canary's central real-substrate finding, confirmed by controlled isolation:

**Setup.** The owner logged into a dedicated Chromium profile (`scratch/issue-84-canary/profile-live`,
gitignored) through a **clean Chrome 151 launch (no automation flags)**, as the issue #16
spike prescribed. After login the profile held a live session (73 cookies, including
`__Secure-next-auth.session-token.*`, `__Secure-next-auth.csrf-token`,
`oai-client-auth-session`, `unified_session_manifest`).

**Isolated observations (same profile, same Chrome 151):**

| Reopen mode | Result |
|---|---|
| Clean Chrome 151 (no automation flags) | Session **recognized**; window shows authenticated "ChatGPT"; no login redirect |
| **Playwright-controlled Chrome 151** (same `executablePath`, adds `--enable-automation` / devtools wiring) | Session **NOT recognized**; page redirects to `accounts.google.com/v3/signin` and `/api/auth/session` becomes unreachable (404) |

**Interpretation.** The OpenAI login gate treats a Playwright/automation-controlled
browser as untrusted and **desauthenticates / forces re-login**, even when the profile
holds a valid persisted session. This confirms, in a real controlled environment and
for the Playwright+Chromium candidate, the transport property the issue #16 CDP spike
had measured for CDP-flag-flagged launches.

**Canary consequence (fail-closed).** Because an authenticated Playwright session
cannot be held/used on chatgpt.com, the Phase 3 live turn **cannot be executed in an
attributable way via Playwright** in this state. Attempting it would land on the login
gate (a `blocked_readiness_reason` / drift), and bypassing automation is forbidden.
This makes the readiness **blocked** for the Playwright live turn and directly informs
whether Playwright+Chromium is usable for the authenticated ChatGPT Web path at all.

**Evidence recorded (sanitized):** `docs/research/issue-84-canary/evidence-auth-gate.md`.
No token/cookie value was logged; presence of session-cookie names and auth redirect
state were the only signals used.

### Corrected bottom line (supersedes the provisional "positive login-gate absence" in §5)

The earlier §5 landing-page observation (no CAPTCHA while logged out) remains true but
is no longer the deciding signal: the deciding signal is that a persisted authenticated
session is **rejected** when the profile is reopened under Playwright. This is a real,
material limitation for the Playwright+Chromium candidate on the authenticated ChatGPT
Web path and should drive the maintainer's ADR/issue-sequencing decision.

---

## 15. Final deliverable checks

- `git status` and `git diff --check` were run (see below): only research documentation under `docs/research/issue-84-canary/` is new.
- No credentials, cookies, tokens, headers, or private content were written into any committed artifact.
- No production code changed.

---

## Sanitization note

This report references only: versions, tool paths, hostnames, and observable readiness
classifications (session-cookie **name presence**, auth redirect state). It contains no
cookie **values**, no tokens, no headers, no conversation content, and no screenshots of
private data. The authenticated session created during this follow-up lives **only** in
the dedicated gitignored profile directory (`scratch/issue-84-canary/profile-live`) and
was never exported, persisted into any artifact, or passed through this agent's evidence.
