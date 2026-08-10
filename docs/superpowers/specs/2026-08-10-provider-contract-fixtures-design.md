# OmniRoute Provider Contract Fixtures Design

**Date:** 2026-08-10
**Issue:** #43
**Base:** `origin/main` at `d087fbfca118fc5d4b51025fea5eec0d5d650253`

## Goal

Replace the largest ad hoc OmniRoute boundary tables with a deterministic, synthetic, redacted corpus that drives the real adapter transport, response parser, error classifier, management evidence checks, delivery-state observations, and the existing attempt-receipt gate.

## Options considered

1. **Keep inline tables and add more cases.** Smallest immediate diff, but it leaves response shapes and management evidence duplicated in Go source and gives future contract drift no single corpus to review.
2. **Build a generic HTTP mock framework.** Flexible, but out of proportion to the provider boundary and risks hiding request-count and redirect behavior behind abstractions.
3. **Use a static manifest plus a focused contract mock.** The manifest names fixtures and expected normalized outcomes. The mock implements only `/v1/chat/completions` and the management endpoints already consumed by the adapter, with explicit transport behaviors. This is the recommended option because it makes the corpus reviewable, keeps phenomena such as timeout and connection reset honest, and preserves exact request-count evidence.

## Architecture

Static files live below `internal/provider/omniroute/testdata/contract/`:

- `manifest.json` is the versioned inventory and scenario index.
- `responses/` contains synthetic completion and HTTP-error bodies, including malformed and empty files.
- `management/` contains safe, missing, ambiguous, incompatible, and telemetry-shape responses.
- `receipts/` contains only synthetic attempt-receipt header values needed for one valid and invalid boundary case.

`contract_fixture_test.go` embeds and validates the corpus, parses the manifest, runs each scenario through the real adapter, calls `Classify` where the operation returns a completion response, validates delivery metadata and rate-limit hints, and compares the manifest inventory with the `ErrorKind` constants parsed from `errors.go`. It also owns the redaction scanner and its negative tests.

`contract_mock_test.go` contains the smallest reusable HTTP harness extracted from the existing `safeHandler`. It records total requests, completion POSTs, management GETs, and redirect replays. It can return configured status, headers, fixture body, receipt header, delay, oversized stream, redirect, or abrupt connection close. A deterministic `http.Transport` dial seam represents generic transport errors and `ECONNRESET` without inventing an HTTP body.

Existing focused tests continue to protect isolated behavior such as exact request wire shape and configuration redaction. The large inline HTTP/body classification table is replaced by the manifest-driven corpus so it does not become a second source of truth.

## Operations and fail-closed boundaries

- `complete_once` exercises the one-request transport/parser seam. It never retries and never rotates accounts.
- `complete` enables the existing receipt-aware path only for synthetic receipt scenarios. It does not alter production behavior or receipt semantics.
- `preflight` exercises all eight current management endpoints. Safe evidence still ends in `unsafe_route` because the current adapter deliberately requires authoritative attempt receipts. Missing, ambiguous, incompatible, and gateway-contract-drift-shaped evidence must fail closed earlier or at route evidence validation.
- `snapshot` exercises the optional rate-limit/resilience telemetry path and expects `telemetry` for malformed management telemetry.

No `ErrorProtocolChanged`, health probe, startup probe, governor policy, retry, account rotation, or live OmniRoute behavior is added. Gateway contract drift is named in the corpus for later #40 reuse but remains represented by the current `RouteSafety`/management evidence mechanism.

## ErrorKind inventory

The manifest inventories every current `ErrorKind` and assigns each to one or more existing producer categories:

- HTTP/body/header: `authentication_expired`, `authentication_denied`, `http_403`, `rate_or_capacity`, `login_challenge`, `captcha`, `suspicious_activity`, `account_warning`, `feature_restriction`, `connection_reset`, `timeout`, `empty_response`, `malformed_json`, `invalid_envelope`, `upstream_server_failure`, `http_status`, `response_too_large`.
- Transport behavior: `transport`, `timeout`, `cancelled`, `connection_reset`, `response_too_large`.
- Local before request: `request_too_large`, `cancelled`, `unsafe_route`, `attempt_receipts_invalid`.
- RouteSafety/management evidence: `unsafe_route`, `telemetry`.
- Attempt receipts: `attempt_receipts_missing`, `attempt_receipts_invalid`.

The inventory test fails on a missing or unknown manifest entry and parses the centralized `ErrorKind` declarations so adding a new typed kind requires an explicit corpus update. `protocol_changed` is intentionally absent because #40 owns that taxonomy.

## Fixture hygiene

The corpus scanner walks every embedded file, rejects high-signal credential fields and headers (`Authorization`, cookies, API keys, access/refresh/session credentials), secret-shaped bearer/API/JWT values, and email-shaped values. It does not reject semantic taxonomy strings such as `token_expired`, opaque request IDs, or synthetic session identifiers solely because they contain the word `token` or `session`.

The scanner is exercised by a negative test using a temporary fixture containing synthetic secret-shaped values. Manifest loading separately rejects malformed manifests, duplicate/unknown scenarios, and references to unknown fixture files. No response body, cookie, credential, prompt, account ID, or session ID is copied from a real account.

## Verification targets

The new contract tests prove normal success, every current adapter error class, rate-limit hints, management evidence outcomes, redirect non-replay, exact single-attempt request counts, bounded timeout/cancellation, receipt validation boundaries, metadata redaction, and fixture hygiene. The normal `go test ./...` path remains the CI gate; `.github/workflows/ci.yml` does not need architectural changes.
