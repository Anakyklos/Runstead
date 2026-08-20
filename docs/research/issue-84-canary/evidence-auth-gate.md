# Issue #84 canary — Evidence: Playwright desauthenticates a persisted session (auth gate)

**Date:** 2026-08-20
**Candidate:** Playwright + Chromium (Chrome 151 via Playwright `executablePath`, engine playwright 1.58.2)
**Status:** sanitized evidence for maintainer review. No cookie/token value was read or written anywhere.

---

## 1. Setup (owner-authenticated dedicated profile)

- Dedicated disposable profile: `scratch/issue-84-canary/profile-live` (gitignored; nothing copied).
- The owner logged in through a **clean Chrome 151 launch** (`--user-data-dir=<profile>`, no
  automation flags) as prescribed by the issue #16 spike.
- After login the profile held a live ChatGPT session. Only **cookie-name presence** was
  observed (names, never values): `__Secure-next-auth.session-token.*` (session),
  `__Secure-next-auth.csrf-token`, `oai-client-auth-session`, `unified_session_manifest`,
  plus tracking/device cookies. Total cookie count grew from 6 (logged out) to 73 (logged in).

## 2. Isolated comparison (same profile, same Chrome 151 binary)

| Reopen mode | URL after load | Session endpoint | Window/title |
|---|---|---|---|
| Clean Chrome 151 (no automation flags) | `https://chatgpt.com/` | — | Authenticated "ChatGPT"; no login redirect |
| Playwright-controlled Chrome 151 (`executablePath=/usr/bin/google-chrome`, adds automation wiring) | redirected to `accounts.google.com/v3/signin/...` | `/api/auth/session` unreachable (HTTP 404 from Google domain) | "Fazer login nas Contas do Google" (login page) |

## 3. Readiness classification for the Playwright live turn

- **verdict:** `blocked_readiness_reason` (auth-gate / drift).
- The OpenAI login gate treats an automation-controlled browser as untrusted and
  **desauthenticates / forces re-login**, even with a valid persisted session in the profile.
- A Phase 3 live turn via Playwright is therefore **not executable in an attributable way**:
  it would land on the login gate; bypassing automation is forbidden (absolute rule).
- **physical model-effect sends:** 0. No turn was dispatched.

## 4. What this evidence does / does not prove

- Proves (real environment, isolated): a persisted authenticated session in the dedicated
  profile is rejected/ignored when reopened by Playwright; the same profile+same Chrome is
  authenticated under a clean (non-automation) launch.
- Does not attempt CAPTCHA/MFA bypass, does not export or read cookie values, does not
  rotate accounts, does not send any message.

## 5. Artifact hygiene

- Only cookie **names**, HTTP status, and redirect host/path are referenced.
- No cookie/token/header value, no account identity, no conversation content appears here.
- The live session remains only in the gitignored profile directory.
