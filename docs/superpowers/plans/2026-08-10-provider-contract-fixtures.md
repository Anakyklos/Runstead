# OmniRoute Provider Contract Fixtures Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans (recommended for inline execution) to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a synthetic, redacted OmniRoute provider-boundary corpus and focused mock harness that exercises the real adapter without live services, retries, account rotation, credentials, internet, or Docker.

**Architecture:** Embed a versioned JSON manifest and static response/management/receipt fixtures under `internal/provider/omniroute/testdata/contract/`. A focused `contractMock` extracted from `safeHandler` serves only the completion and management routes consumed by Runstead, records exact request counts, and exposes deterministic delay, redirect, oversized-stream, and connection-failure behaviors. Manifest-driven tests run `completeOnce`, the existing receipt-aware `Complete`, `Preflight`, `Snapshot`, and `Classify` without changing production policy.

**Tech Stack:** Go standard library (`embed`, `encoding/json`, `go/parser`, `io/fs`, `net/http/httptest`, `testing`), existing provider/governor adapter types, static JSON fixtures, `go test ./...`.

## Global Constraints

- Base the branch on the freshly fetched `origin/main` at `d087fbfca118fc5d4b51025fea5eec0d5d650253` or a newer fetched `origin/main`.
- Do not add `ErrorProtocolChanged`, health probes, startup probes, new governor policy, retries, fallback/account rotation, live OmniRoute behavior, or Docker-dependent tests.
- Do not create a generic HTTP mock framework; implement only the routes and behaviors consumed by this adapter.
- Every fixture must be synthetic and redacted. No credentials, cookies, prompts, emails, personal identifiers, or captured session values may be added.
- The hygiene scanner must reject credential fields/headers and secret-shaped values without rejecting semantic strings such as `token_expired`.
- The existing `RouteSafety`, delivery-state, and attempt-receipt semantics remain unchanged.
- The normal CI path remains `go test ./...`; do not redesign `.github/workflows/ci.yml`.
- Use TDD: write a failing test, observe the intended failure, implement the smallest change, and re-run focused tests before each refactor.

---

### Task 1: Define the corpus schema, hygiene gate, and inventory tests

**Files:**
- Create: `internal/provider/omniroute/contract_fixture_test.go`
- Create: `internal/provider/omniroute/testdata/contract/manifest.json`
- Create: `internal/provider/omniroute/testdata/contract/responses/`
- Create: `internal/provider/omniroute/testdata/contract/management/`
- Create: `internal/provider/omniroute/testdata/contract/receipts/`

**Interfaces:**
- `contractManifest` loads the manifest with `SchemaVersion`, `ManagementDefaults`, `ErrorKindInventory`, and `Scenarios`.
- `loadContractManifest(fsys fs.FS) (contractManifest, error)` rejects malformed JSON, unsupported schema versions, duplicate scenario names, unknown operation/transport values, unknown fixture references, missing expected inventory entries, and unknown inventory entries.
- `scanFixtureHygiene(fsys fs.FS) error` walks every file and rejects high-signal credential fields/headers, secret-shaped bearer/API/JWT values, and email-shaped values while allowing `token_expired`, opaque request IDs, and synthetic session IDs.
- `errorKindsFromSource() (map[ErrorKind]struct{}, error)` parses `errors.go` with `go/parser` and discovers every typed `ErrorKind` constant.
- `runContractScenario(t *testing.T, manifest contractManifest, scenario contractScenario)` is the scenario entrypoint used by later tasks.

- [ ] **Step 1: Write failing tests for the corpus loader and hygiene behavior.** Add tests named `TestContractFixtureHygieneRejectsSecretShapedValues`, `TestContractManifestRejectsUnknownFixture`, `TestContractManifestRejectsMalformedManifest`, `TestContractManifestRejectsUnknownScenarioReference`, `TestContractManifestInventoryMatchesErrorKinds`, and `TestContractFixtureHygieneAllowsSemanticTokenSignals`. Use `t.TempDir()` and `os.DirFS` for negative fixtures so no secret-shaped value enters git history.

```go
func TestContractFixtureHygieneRejectsSecretShapedValues(t *testing.T) {
    dir := t.TempDir()
    writeFixtureFile(t, dir, "responses/unsafe.json", `{"Authorization":"Bearer synthetic-secret-value-123456"}`)
    if err := scanFixtureHygiene(os.DirFS(dir)); err == nil {
        t.Fatal("scanFixtureHygiene() accepted a secret-shaped fixture")
    }
}
```

- [ ] **Step 2: Run the focused tests and confirm they fail for missing loader/scanner behavior.**

Run: `go test -count=1 ./internal/provider/omniroute -run 'TestContractFixtureHygiene|TestContractManifest'`

Expected: FAIL because the corpus test helpers and manifest are not implemented yet, not because of a syntax or environment error.

- [ ] **Step 3: Implement the manifest types, embedded filesystem, loader validation, hygiene scanner, source inventory parser, and delivery-state/error-kind parsing helpers.** Keep all validation test-only and use `fs.WalkDir`, `json.Decoder.DisallowUnknownFields` for the manifest, explicit allowed operation/transport/category sets, and high-signal field/value checks. Do not reject the substring `token` by itself.

- [ ] **Step 4: Add the complete synthetic manifest and static files.** Include one inventory entry for each current `ErrorKind`: `transport`, `timeout`, `cancelled`, `authentication_expired`, `authentication_denied`, `http_403`, `rate_or_capacity`, `login_challenge`, `captcha`, `suspicious_activity`, `account_warning`, `feature_restriction`, `connection_reset`, `empty_response`, `malformed_json`, `invalid_envelope`, `upstream_server_failure`, `http_status`, `response_too_large`, `request_too_large`, `unsafe_route`, `telemetry`, `attempt_receipts_missing`, and `attempt_receipts_invalid`. Use scenario names that later #40 can reuse for gateway shape drift, but do not add a protocol-changed kind.

- [ ] **Step 5: Run the loader/hygiene tests and commit the corpus foundation.**

Run: `gofmt -w internal/provider/omniroute/contract_fixture_test.go && go test -count=1 ./internal/provider/omniroute -run 'TestContractFixtureHygiene|TestContractManifest'`

Expected: PASS for the loader, inventory, and hygiene tests.

Commit: `git add internal/provider/omniroute/contract_fixture_test.go internal/provider/omniroute/testdata/contract && git commit -m "test(provider): add OmniRoute contract corpus inventory"`

### Task 2: Extract the focused mock harness and preserve existing test seams

**Files:**
- Create: `internal/provider/omniroute/contract_mock_test.go`
- Modify: `internal/provider/omniroute/client_test.go`
- Modify: `internal/provider/omniroute/review_regressions_test.go` only if shared helper names require it

**Interfaces:**
- `contractMock` serves `/v1/chat/completions`, the eight management paths in `client.go`, and a redirect target used only to detect accidental replay.
- `newContractMockServer(t *testing.T, config contractMockConfig) *contractMockServer` starts an `httptest.Server`, loads configured fixture bodies, and exposes `Close`, `BaseURL`, `Counts`, and recorded request metadata.
- `safeHandler(chat http.HandlerFunc) http.Handler` remains available to existing focused tests but delegates to the extracted management route implementation.
- `testConfig(baseURL string) Config` and `newTransportClient` move to the shared test helper file. The configured key must be an obviously synthetic value such as `fixture-api-key`.

- [ ] **Step 1: Add a failing harness test for exact request counts and redirect behavior.** Add `TestContractMockCountsRedirectWithoutReplay` with a 307 response and assert one completion POST and zero redirect-target requests.

- [ ] **Step 2: Run that test and confirm it fails because the extracted harness does not exist.**

Run: `go test -count=1 ./internal/provider/omniroute -run TestContractMockCountsRedirectWithoutReplay`

Expected: FAIL with undefined harness symbols or the missing count assertion.

- [ ] **Step 3: Move the existing `safeResilienceResponse`, `testConfig`, `newTransportClient`, and `safeHandler` behavior into `contract_mock_test.go`.** Load safe management bodies from the static corpus instead of duplicating their JSON in Go. Retain the existing callback shape so focused tests continue to inspect request headers and bodies.

- [ ] **Step 4: Implement explicit response behaviors.** Support configured status, headers, fixture body, attempt-receipt header fixture, redirect `Location`, delayed response until request context cancellation, oversized synthetic stream, and connection close. Use request counters keyed by total, completion POST, management GET, and redirect replay. Add deterministic `http.Transport` dial errors for generic transport and `syscall.ECONNRESET` scenarios.

- [ ] **Step 5: Run the focused harness and existing OmniRoute tests.**

Run: `gofmt -w internal/provider/omniroute/contract_mock_test.go internal/provider/omniroute/client_test.go && go test -count=1 ./internal/provider/omniroute`

Expected: PASS, with the existing request-wire, redaction, delivery, safety, telemetry, and receipt tests still green.

- [ ] **Step 6: Commit the harness extraction.**

Commit: `git add internal/provider/omniroute/contract_mock_test.go internal/provider/omniroute/client_test.go internal/provider/omniroute/review_regressions_test.go && git commit -m "test(provider): extract focused OmniRoute contract mock"`

### Task 3: Run the full manifest through the real adapter boundary

**Files:**
- Modify: `internal/provider/omniroute/contract_fixture_test.go`
- Modify: `internal/provider/omniroute/contract_mock_test.go` if scenario configuration needs a focused addition
- Modify: `internal/provider/omniroute/client_test.go` to remove the superseded large inline HTTP/body classification table

**Interfaces:**
- `TestContractCorpusScenarios` loads the embedded manifest, runs each scenario, checks the real `ErrorKind`, `Classify` outcome, delivery state, metadata hints, and exact request counts.
- `contractScenario` supports `complete_once`, receipt-aware `complete`, `preflight`, and `snapshot` operations. Transport modes include `dial_error`, `connection_reset`, `delay`, `redirect`, `oversized_response`, and `close_connection`.
- The expected outcome records error kind, normalized class where applicable, upstream reached, delivery state, Retry-After, reset time, response text, and request counts.

- [ ] **Step 1: Add the manifest-driven scenario test before wiring it to the harness.** Make the test iterate every manifest scenario and assert the expected fields. Include explicit assertions that the configured API key is absent from errors, raw `Authorization` is absent from metadata, raw session identifiers are replaced by the existing hash form, and the synthetic prompt/fixture content is never present in failure strings.

- [ ] **Step 2: Run the scenario test to observe the expected red state.**

Run: `go test -count=1 ./internal/provider/omniroute -run TestContractCorpusScenarios`

Expected: FAIL for missing scenario execution/configuration, not for an unrelated package compile failure.

- [ ] **Step 3: Implement scenario setup and assertions.** Use a fixed clock for rate-limit/reset expectations. For normal and HTTP/body scenarios call `completeOnce`; for receipt scenarios configure `provider.ReceiptRouteSafety`, synthetic provider/model/lane values, and a deterministic client request ID before calling `Complete`; for management scenarios assert fail-closed `unsafe_route`/`telemetry` without treating management evidence as authorization. Check every completion request count equals the manifest expectation.

- [ ] **Step 4: Include the required scenario families.** Cover success, expired/denied authentication, 403, login challenge, CAPTCHA, suspicious activity, account warning, feature restriction, rate/capacity with `Retry-After`, connection reset body and transport, timeout status and bounded context timeout, cancellation before dispatch, empty/malformed/invalid completion envelopes, upstream failure, HTTP redirect, oversized response, oversized request, safe management evidence, missing/ambiguous/incompatible management evidence, gateway-contract-drift-shaped evidence, malformed telemetry, valid/missing/invalid attempt receipts.

- [ ] **Step 5: Remove or adapt the large inline classification table.** Keep small tests that protect isolated request-wire, redirect, timeout, body-limit, and config-redaction behavior. Let the manifest be the single source of truth for status/body/expected kind cases.

- [ ] **Step 6: Run focused red-green verification.**

Run: `gofmt -w internal/provider/omniroute/contract_fixture_test.go internal/provider/omniroute/client_test.go && go test -count=1 ./internal/provider/omniroute -run 'TestContractCorpusScenarios|TestContractFixtureHygiene|TestContractManifest'`

Expected: PASS for the entire corpus and all negative loader/hygiene tests.

- [ ] **Step 7: Commit the integrated contract scenarios.**

Commit: `git add internal/provider/omniroute/contract_fixture_test.go internal/provider/omniroute/client_test.go internal/provider/omniroute/contract_mock_test.go internal/provider/omniroute/testdata/contract && git commit -m "test(provider): exercise OmniRoute boundary from redacted fixtures"`

### Task 4: Add short contributor documentation and inspect fixture hygiene

**Files:**
- Modify: `docs/development.md` only if the existing workflow has a suitable provider-test section; otherwise add a short comment in `contract_fixture_test.go` and do not modify docs.
- Inspect: `internal/provider/omniroute/testdata/contract/**`

- [ ] **Step 1: Add a concise note explaining the corpus location, manifest scenario format, how to add a fixture, and the hygiene rules.** Preserve the native/Docker development workflow established by #15 and do not add a second CI pipeline.

- [ ] **Step 2: Run a deterministic source/data inspection.**

Run: `find internal/provider/omniroute/testdata/contract -type f -print0 | xargs -0 grep -nE 'Authorization|Bearer |Set-Cookie|Cookie|api[_-]?key|access[_-]?token|refresh[_-]?token|session[_-]?token|@[A-Za-z0-9.-]+\.[A-Za-z]{2,}' || true`

Expected: no matches in committed fixture data. Semantic `token_expired` remains allowed and is checked by the Go hygiene test rather than a blanket grep.

- [ ] **Step 3: Commit documentation if changed.**

Commit: `git add docs/development.md internal/provider/omniroute/contract_fixture_test.go && git commit -m "docs(provider): document OmniRoute contract fixture hygiene"`

### Task 5: Full verification, review, and PR preparation

**Files:**
- Inspect all changes from `origin/main` to `HEAD`.
- Modify only files needed to fix failures found during verification.

- [ ] **Step 1: Refresh `origin/main` and confirm no unexpected advancement before final integration.** If `origin/main` advanced, rebase or merge the latest base without force-pushing and rerun the full suite.

- [ ] **Step 2: Run every required verification command.**

```bash
test -z "$(gofmt -l .)"
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/runstead
bash experiments/protocol/test.sh
git diff --check
go test -count=1 ./internal/provider/...
go test -race -count=1 ./internal/provider/...
```

If Docker is available, additionally run:

```bash
docker compose run --rm dev go test ./internal/provider/...
```

Record only commands actually executed and their exit status in the final PR body.

- [ ] **Step 3: Review the complete diff and fixture contents.** Verify no `.github/workflows/ci.yml` change was needed, no production adapter semantics changed, no `protocol_changed` taxonomy was introduced, no live behavior is enabled, and all fixture paths are covered by the manifest.

- [ ] **Step 4: Request an independent code review using the final base and head SHAs.** Fix all critical/important findings, rerun affected tests, and commit fixes separately when useful.

- [ ] **Step 5: Push the dedicated branch and open a non-draft PR to `main`.** Use title `test(provider): add redacted OmniRoute contract fixtures`. The body must contain `Closes #43` and summarize corpus format, focused mock/harness, ErrorKind inventory, hygiene rules, single-attempt/redirect evidence, commands actually run, and the deliberate #40 boundary. Do not manually close the issue.
