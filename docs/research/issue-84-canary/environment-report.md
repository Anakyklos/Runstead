# Issue #84 Canary — Environment Report (Phase 0)

**Date:** 2026-08-20
**Status:** Readiness inspection only. No code modified, no model turn executed.
**Candidate under canary:** Playwright + Chromium (minimum assisted canary, PR #85 / Issue #84).
**Author role:** local execution agent (not maintainer). Evidence is collected and sanitized for maintainer review only.

---

## 1. Operating system

| Item | Value |
|---|---|
| OS | Linux Mint 22.3 (Zena) |
| Distributor ID | linuxmint |
| ID_LIKE | ubuntu debian |
| Kernel | 7.0.0-28-generic (Ubuntu 24.04 toolchain) |
| Architecture | x86_64 |
| Host | SAMSUNG 550XED (12th Gen Intel i5-1235U) |
| Memory | 31.0 GiB |

---

## 2. Chromium / Chrome availability

| Item | Value |
|---|---|
| System Chrome binary | `/usr/bin/google-chrome` (stable) |
| System Chrome version | 151.0.7922.71 |
| Playwright-managed Chromium | `~/.cache/ms-playwright/chromium-1208/chrome-linux64/chrome` |
| Playwright Chromium revision | 1208 |
| Playwright Chromium version | Google Chrome for Testing 145.0.7632.6 |
| headless shell | present in `chromium-1208` cache (revision bundles) |
| Browser cache root | `~/.cache/ms-playwright/` |
| Installation marker | `INSTALLATION_COMPLETE` present |

---

## 3. Node / npm / Playwright

| Item | Value |
|---|---|
| Node | v24.19.0 (nvm) |
| npm | 11.17.0 |
| Playwright (usable) | 1.58.2 @ `/home/pedro/.hermes/hermes-agent/node_modules/playwright` (matches chromium-1208) |
| Playwright (present, browser-mismatched) | 1.62.1 @ `/home/pedro/.omniroute/runtime/node_modules/playwright` (expects revision 1234 / Chrome 151, not the 1208 cache) |
| Playwright in repo `node_modules` | none (repo has no `node_modules`) |
| Playwright on PATH / global | not installed as a global npm module |

**Note on browser/engine pairing:** the only downloaded Playwright Chromium in the cache (`chromium-1208`, Chrome for Testing 145) is the browser expected by Playwright 1.58.2 (Hermes). The 1.62.1 installation points at a different revision (1234). The canary therefore uses the 1.58.2 engine with its matching local browser. No browser download was performed.

---

## 4. Repository state

| Item | Value |
|---|---|
| Branch | `research/issue-16-standalone-browser-spike` |
| Working tree | clean (nothing to commit) |
| Remote `origin` | github.com/RenyEnnos/Runstead.git |
| Remote `upstream` | github.com/pedro-labsabs/Runstead.git |
| Repo root | `/home/pedro/Documentos/codigo/Runstead` |
| Existing research | `docs/research/first-party-chatgpt-web-standalone-spike.md` (CDP substrate, issue #16) |

No production code or config was modified by this inspection.

---

## 5. Available Chromium profiles

| Profile | Authenticated? | Disposable? | Notes |
|---|---|---|---|
| `experiments/first-party-chatgpt-web-standalone/profiles/standalone-spike` | Yes (session resident in profile; prior CDP spike, issue #16) | Partially (spike-owned, gitignored) | Belongs to a prior experiment. Task rules forbid copying/reusing a real profile and require a profile created specifically for this canary, so it is **not** reused here. |
| Fresh dedicated canary profile | No (to be created) | Yes (created specifically for this test) | Used by Phase 1. No real user data copied in. |

---

## 6. Authorized authenticated session

**Unknown at inspection time.** No separate authorized session identifier was provided to this agent for the Playwright+Chromium canary. The only authenticated session on this machine is the one owned by the prior CDP spike profile, which the task explicitly forbids reusing/copying.

Per the task's conservative rule, this is registered as **UNKNOWN** rather than assumed available.

---

## 7. Network reachability (control-plane probe, no auth attempted)

| Target | Result |
|---|---|
| `https://chatgpt.com/` | Reachable (HTTP 403 to plain curl — expected without a real browser session) |
| `https://auth.openai.com/` | Reachable (HTTP 403 to plain curl) |

No login, no CAPTCHA solving, no session was attempted during this probe.

---

## 8. Prior measured property relevant to this canary (from issue #16 CDP spike)

The checked-in standalone spike recorded a measured transport property that is directly relevant to the Playwright candidate:

> OpenAI's login gate (Cloudflare Turnstile on auth.openai.com + Auth0 "browser may not be secure") challenges Chrome launches that carry remote-debugging / CDP flags. A clean (flag-free) launch logs in fine. No automation concealment was used.

Playwright launches Chromium **with CDP/remote-debugging wiring by design**. This canary's Phase 1 therefore explicitly probes whether the Playwright-launched Chromium is challenged by the login gate, using a fresh disposable profile. This is the core substrate property under test.

Reference: `docs/research/first-party-chatgpt-web-standalone-spike.md` and `experiments/first-party-chatgpt-web-standalone/evidence/login-enrollment.json`.

---

## 9. Limitations found

1. No dedicated authorized session was provided; authenticated-turn validation depends on readiness findings.
2. The only usable Playwright engine is 1.58.2 (Hermes); the 1.62.1 install's expected browser (rev 1234) is not downloaded, so it cannot run this canary's live Chromium.
3. Repo has no `node_modules`; the canary harness references an external Playwright install and adds **no** dependency to the project.
4. No global Playwright CLI is on PATH.
5. Headless vs headed launch differences matter for the login gate; the canary records which launch mode is observed against the gate.
6. Existing authenticated profile is not reusable per task rules (no copying real profiles; profile must be created for the test).

---

## 10. Confidence and disposition

- Environment inspection is non-destructive; no files changed other than this report.
- Next phase (readiness) will use a fresh dedicated disposable profile, will not attempt login, and will stop and record `blocked_readiness_reason` if a CAPTCHA / MFA / challenge / password prompt appears.
- No secrets were read into or placed in this artifact.
