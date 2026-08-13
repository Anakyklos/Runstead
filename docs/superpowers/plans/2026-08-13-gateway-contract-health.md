# OmniRoute Gateway-Contract Health Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an on-demand, read-only, bounded, fail-closed OmniRoute gateway-contract health probe for `/api/providers`, `/api/settings`, and `/api/models/alias`, without weakening RouteSafety, governor admission, attempt receipts, or accounting.

**Architecture:** Add a provider-neutral `GatewayContractHealth` state/result and optional `ContractHealthAware` capability. OmniRoute owns the bounded GET-only probe, shape validation, sanitized latest result, and direct completion gate. `Governor.Execute` consumes the optional capability as an additional gate and emits the result through existing admission events; `RouteSafety`, receipts, persistence, policy, budgets, circuits, and accounting remain independent.

**Tech Stack:** Go standard library, existing `net/http` adapter, existing `httptest` contract mock and embedded synthetic corpus, existing governor event sink and `internal/trace` slog renderer.

## Global Constraints

- Probe exactly `/api/providers`, `/api/settings`, and `/api/models/alias`; no `/v1/chat/completions` access.
- GET only; no model POST, writes, cookies, redirects/replay, retry, fallback, rotation, scheduler, daemon, or background polling.
- Reuse `internal/provider/omniroute/contract_mock_test.go` and `testdata/contract`; do not add a fixture framework.
- Use existing timeout, response-body limit, redirect rejection, and HTTP seam.
- `GatewayContractHealthUnknown` is the zero value and all non-healthy states block protected execution.
- Do not add an OmniRoute-specific import to `internal/governor` and do not put mutable health in `RouteSafety`.
- Do not add a completion `ErrorKind` for gateway health; use typed health state plus provider-neutral admission/error surfaces.
- Gateway management health never proves ChatGPT Web/upstream health or authoritative attempt accounting.
- No raw management bodies, prompts, responses, authorization, API keys, cookies, session IDs, account IDs, or arbitrary remote error text in results, events, logs, or tests.
- No new external dependency.

---

### Task 1: Add provider-neutral health types and governor admission surface

**Files:**
- Modify: `internal/provider/provider.go`
- Modify: `internal/governor/types.go`
- Modify: `internal/governor/telemetry.go`
- Test: `internal/provider/provider_health_test.go`
- Test: `internal/governor/gateway_contract_health_test.go`

**Interfaces:**
- Produces `provider.GatewayContractHealth`, `provider.GatewayContractHealthResult`, `provider.ContractHealthAware`, and `provider.ErrGatewayContractUnhealthy`.
- Produces `governor.AdmissionGatewayContractUnhealthy` and optional sanitized health fields on `AdmissionResult` and `Event`.
- Does not change `RouteSafety` or the `TelemetrySource` interface.

- [ ] **Step 1: Write failing provider health type tests**

```go
func TestGatewayContractHealthZeroValueIsUnknown(t *testing.T) {
    var state provider.GatewayContractHealth
    if state != provider.GatewayContractHealthUnknown {
        t.Fatalf("zero health = %v, want unknown", state)
    }
    if state.String() != "unknown" {
        t.Fatalf("zero health string = %q, want unknown", state.String())
    }
}

func TestGatewayContractHealthResultHealthyOnlyForHealthyState(t *testing.T) {
    result := provider.GatewayContractHealthResult{State: provider.GatewayContractHealthHealthy}
    if !result.Healthy() {
        t.Fatal("healthy result did not report Healthy")
    }
    result.State = provider.GatewayContractHealthDegraded
    if result.Healthy() {
        t.Fatal("degraded result reported Healthy")
    }
}
```

- [ ] **Step 2: Run the provider test and verify the expected missing-symbol failure**

Run: `go test ./internal/provider -run 'TestGatewayContractHealth' -count=1`
Expected: FAIL because the health types and methods do not exist yet.

- [ ] **Step 3: Implement the minimal provider-neutral types**

Add to `internal/provider/provider.go`:

```go
var ErrGatewayContractUnhealthy = errors.New("provider gateway contract is not healthy")

type GatewayContractHealth uint8

const (
    GatewayContractHealthUnknown GatewayContractHealth = iota
    GatewayContractHealthHealthy
    GatewayContractHealthDegraded
    GatewayContractHealthProtocolChanged
)

func (h GatewayContractHealth) String() string {
    switch h {
    case GatewayContractHealthHealthy:
        return "healthy"
    case GatewayContractHealthDegraded:
        return "degraded"
    case GatewayContractHealthProtocolChanged:
        return "protocol_changed"
    default:
        return "unknown"
    }
}

type GatewayContractHealthResult struct {
    State      GatewayContractHealth
    ReasonCode string
    Endpoint   string
    CheckedAt  time.Time
}

func (r GatewayContractHealthResult) Healthy() bool {
    return r.State == GatewayContractHealthHealthy
}

type ContractHealthAware interface {
    GatewayContractHealth() GatewayContractHealthResult
}
```

The existing `errors` and `time` imports already exist in this file. Keep the type provider-neutral and do not refer to OmniRoute paths or error kinds.

- [ ] **Step 4: Add the provider-neutral admission/event fields and code**

In `internal/governor/types.go`, add an admission code:

```go
AdmissionGatewayContractUnhealthy AdmissionCode = "gateway_contract_unhealthy"
```

Add optional result fields without changing the existing telemetry contract:

```go
type AdmissionResult struct {
    // existing fields...
    GatewayContractHealth *provider.GatewayContractHealthResult
}

type Event struct {
    // existing fields...
    GatewayContractHealth *provider.GatewayContractHealthResult
}
```

In `internal/governor/telemetry.go`, copy the optional result into the existing admission event in `emitAdmissionLocked`. Do not persist it in `PersistedState`, `PersistedTelemetry`, or `RouteSafety`.

- [ ] **Step 5: Run the focused tests and verify green**

Run: `go test ./internal/provider ./internal/governor -run 'TestGatewayContractHealth' -count=1`
Expected: PASS for the zero-value/type tests; the governor package may still have no execution gate test until Task 3.

- [ ] **Step 6: Commit the provider-neutral surface**

```bash
git add internal/provider/provider.go internal/provider/provider_health_test.go internal/governor/types.go internal/governor/telemetry.go internal/governor/gateway_contract_health_test.go
git commit -m "feat(provider): define gateway contract health state"
```

---

### Task 2: Implement the bounded OmniRoute probe and deterministic classifications

**Files:**
- Create: `internal/provider/omniroute/gateway_contract_health.go`
- Modify: `internal/provider/omniroute/client.go` to add the synchronized latest-result field
- Test: `internal/provider/omniroute/gateway_contract_health_test.go`
- Reuse unchanged: `internal/provider/omniroute/contract_mock_test.go`
- Reuse unchanged: `internal/provider/omniroute/contract_fixture_test.go`
- Reuse unchanged: `internal/provider/omniroute/testdata/contract/management/*.json`

**Interfaces:**
- Consumes `Client.config`, `Client.getTelemetry`, `Client.now`, `jsonObject`, and the existing contract mock/fixtures.
- Produces `(*Client).ProbeGatewayContract(context.Context) provider.GatewayContractHealthResult` and `(*Client).GatewayContractHealth() provider.GatewayContractHealthResult`.
- Implements `provider.ContractHealthAware` on `*Client`.

- [ ] **Step 1: Add focused RED tests for initial state and healthy corpus**

Add tests in `gateway_contract_health_test.go` using `newContractMockServer` and `safeContractManagementResponses`:

```go
func TestGatewayContractHealthStartsUnknown(t *testing.T) {
    server := newContractMockServer(t, contractMockConfig{})
    defer server.Close()
    client, err := New(testConfig(server.URL()), Options{HTTPClient: server.Client()})
    if err != nil { t.Fatal(err) }
    got := client.GatewayContractHealth()
    if got.State != provider.GatewayContractHealthUnknown { t.Fatalf("initial health = %#v", got) }
}

func TestProbeGatewayContractRecognizesThreeManagementFixtures(t *testing.T) {
    server := newContractMockServer(t, contractMockConfig{})
    defer server.Close()
    client, err := New(testConfig(server.URL()), Options{HTTPClient: server.Client()})
    if err != nil { t.Fatal(err) }
    got := client.ProbeGatewayContract(context.Background())
    if got.State != provider.GatewayContractHealthHealthy { t.Fatalf("probe health = %#v", got) }
    counts := server.Counts()
    if counts["management_gets"] != 3 || counts["chat_posts"] != 0 || counts["total"] != 3 {
        t.Fatalf("probe counts = %#v, want exactly three management GETs and no chat POST", counts)
    }
}
```

Add cases for `settings-shape-drift.json`, structurally missing/incompatible provider fields (inconsistent `total`, missing `isActive`), a non-array `wildcardAliases`, 404, 410, malformed JSON, and malformed shape. Each case must assert the typed state and that `chat_posts == 0`. Ambiguous or provider/model-incompatible *configuration* is not a gateway-contract case: it stays a `RouteSafety`/preflight refusal.

- [ ] **Step 2: Run the RED tests and confirm they fail for missing probe methods**

Run: `go test ./internal/provider/omniroute -run 'Test(GatewayContractHealth|ProbeGatewayContract)' -count=1`
Expected: FAIL because the probe and result storage do not exist.

- [ ] **Step 3: Implement fixed endpoint order and result storage**

Add `gatewayContractHealth provider.GatewayContractHealthResult` to `Client` in `client.go`, leaving its zero value untouched so a new client is unprobed/`unknown`. Create `gateway_contract_health.go` with a fixed endpoint list:

```go
var gatewayContractEndpoints = [...]string{providersPath, settingsPath, modelAliasesPath}

func (c *Client) GatewayContractHealth() provider.GatewayContractHealthResult {
    if c == nil { return provider.GatewayContractHealthResult{} }
    c.mu.RLock()
    defer c.mu.RUnlock()
    return c.gatewayContractHealth
}

func (c *Client) ProbeGatewayContract(ctx context.Context) provider.GatewayContractHealthResult {
    if c == nil { return provider.GatewayContractHealthResult{} }
    checkedAt := c.now()
    result := provider.GatewayContractHealthResult{State: provider.GatewayContractHealthUnknown, CheckedAt: checkedAt}
    if err := ctx.Err(); err != nil {
        result.ReasonCode = gatewayContractReasonContext
        return c.recordGatewayContractHealth(result)
    }
    for _, endpoint := range gatewayContractEndpoints {
        body, metadata, err := c.getTelemetry(ctx, endpoint)
        if err != nil {
            result = classifyGatewayContractHTTPOrTransport(endpoint, metadata, err, checkedAt)
            return c.recordGatewayContractHealth(result)
        }
        if reasonCode := validateGatewayContractEndpoint(endpoint, body); reasonCode != "" {
            result = provider.GatewayContractHealthResult{
                State: provider.GatewayContractHealthProtocolChanged,
                ReasonCode: reasonCode,
                Endpoint: endpoint,
                CheckedAt: checkedAt,
            }
            return c.recordGatewayContractHealth(result)
        }
    }
    result.State = provider.GatewayContractHealthHealthy
    result.ReasonCode = gatewayContractReasonRecognized
    return c.recordGatewayContractHealth(result)
}
```

`recordGatewayContractHealth` must copy only the typed result under `c.mu`; never store body bytes or remote error text. If a nil context is considered invalid by the existing adapter convention, keep that convention rather than inventing a second context policy.

- [ ] **Step 4: Implement strict shape validators without reusing route authorization claims**

Use `jsonObject` and `json.RawMessage` helpers, but keep these validators separate from `safeRouteEvidence`. `validateGatewayContractEndpoint` returns an empty string for a recognized shape or one of the fixed reason-code constants listed below; it never returns an `error` whose text could contain remote data:

- `/api/providers`: require an object with a `connections` array and an integer `total` equal to the connection count; each connection must have a string `provider` and a bool `isActive`. This is structural only: `defaultModel` is nullable and is deliberately ignored, and provider/model selection or active-match counting is never a gateway-contract concern. Nullable, differing, or missing `defaultModel` values and ambiguous/incompatible active-connection configuration are owned by `RouteSafety`/preflight, not by this probe.
- `/api/settings`: require object fields `wildcardAliases` (array of `{pattern,target}` objects per the OmniRoute schema), `modelAliases` (object), and `globalFallbackModel` (string). Validate field types only: a non-empty `wildcardAliases` array is schema-valid and is not contract drift; whether aliases make the protected route unsafe is a `RouteSafety` decision.
- `/api/models/alias`: require object field `aliases` (object). Do not claim that an empty alias map proves route safety.

Use fixed reason codes such as `recognized`, `context_cancelled`, `timeout`, `transport_uncertain`, `http_404`, `http_410`, `temporary_http_status`, `malformed_json`, and `missing_or_invalid_field`. Do not return `err.Error()` as a reason.

- [ ] **Step 5: Implement HTTP classification and preserve existing bounded transport behavior**

Classify without changing `getTelemetry`:

```go
func classifyGatewayContractHTTPOrTransport(endpoint string, metadata provider.ResponseMetadata, err error, checkedAt time.Time) provider.GatewayContractHealthResult {
    if errors.Is(err, context.Canceled) {
        return healthResult(provider.GatewayContractHealthUnknown, gatewayContractReasonContext, endpoint, checkedAt)
    }
    if errors.Is(err, context.DeadlineExceeded) {
        return healthResult(provider.GatewayContractHealthUnknown, gatewayContractReasonTimeout, endpoint, checkedAt)
    }
    switch metadata.StatusCode {
    case http.StatusNotFound:
        return healthResult(provider.GatewayContractHealthProtocolChanged, gatewayContractReasonHTTP404, endpoint, checkedAt)
    case http.StatusGone:
        return healthResult(provider.GatewayContractHealthProtocolChanged, gatewayContractReasonHTTP410, endpoint, checkedAt)
    case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
        return healthResult(provider.GatewayContractHealthDegraded, gatewayContractReasonTemporaryHTTP, endpoint, checkedAt)
    default:
        return healthResult(provider.GatewayContractHealthUnknown, gatewayContractReasonTransport, endpoint, checkedAt)
    }
}
```

A redirect response is not followed because `New` already installs `http.ErrUseLastResponse`; it remains a single bounded GET and is classified non-healthy. A response-body limit/read error never includes body data in the result.

- [ ] **Step 6: Run all probe tests and verify green**

Run: `go test ./internal/provider/omniroute -run 'Test(GatewayContractHealth|ProbeGatewayContract)' -count=1`
Expected: PASS, with request counts showing at most three management GETs per probe and zero completion POSTs.

- [ ] **Step 7: Commit the probe implementation**

```bash
git add internal/provider/omniroute/gateway_contract_health.go internal/provider/omniroute/gateway_contract_health_test.go
git commit -m "feat(provider): probe OmniRoute gateway contract"
```

---

### Task 3: Enforce health fail-closed in direct adapter and governor execution

**Files:**
- Modify: `internal/provider/omniroute/client.go`
- Modify: `internal/governor/execute.go`
- Test: `internal/provider/omniroute/gateway_contract_health_test.go`
- Test: `internal/governor/gateway_contract_health_test.go`

**Interfaces:**
- Consumes `provider.ContractHealthAware` and `provider.ErrGatewayContractUnhealthy`.
- Produces no change to the `provider.Client` interface and no OmniRoute import in governor.

- [ ] **Step 1: Add RED direct-execution tests**

Add cases with receipt-aware OmniRoute configuration so the health gate is the first relevant blocker:

```go
func TestCompleteBlocksUnknownGatewayContractHealth(t *testing.T) {
    client, server := newReceiptAwareHealthTestClient(t, nil)
    defer server.Close()
    _, err := client.Complete(context.Background(), provider.Request{Prompt: "synthetic"})
    if !errors.Is(err, provider.ErrGatewayContractUnhealthy) { t.Fatalf("Complete() error = %v", err) }
    if got := server.Counts()["chat_posts"]; got != 0 { t.Fatalf("chat POSTs = %d, want 0", got) }
}

func TestCompleteBlocksProtocolChangedGatewayContractHealth(t *testing.T) {
    client, server := newReceiptAwareHealthTestClient(t, map[string]contractMockResponse{
        settingsPath: {status: http.StatusOK, body: mustReadContractFixture("management/settings-shape-drift.json")},
    })
    defer server.Close()
    health := client.ProbeGatewayContract(context.Background())
    if health.State != provider.GatewayContractHealthProtocolChanged { t.Fatalf("health = %#v", health) }
    _, err := client.Complete(context.Background(), provider.Request{Prompt: "synthetic"})
    if !errors.Is(err, provider.ErrGatewayContractUnhealthy) { t.Fatalf("Complete() error = %v", err) }
    if got := server.Counts()["chat_posts"]; got != 0 { t.Fatalf("chat POSTs = %d, want 0", got) }
}

func TestCompleteBlocksDegradedGatewayContractHealth(t *testing.T) {
    client, server := newReceiptAwareHealthTestClient(t, map[string]contractMockResponse{
        providersPath: {status: http.StatusServiceUnavailable},
    })
    defer server.Close()
    health := client.ProbeGatewayContract(context.Background())
    if health.State != provider.GatewayContractHealthDegraded { t.Fatalf("health = %#v", health) }
    _, err := client.Complete(context.Background(), provider.Request{Prompt: "synthetic"})
    if !errors.Is(err, provider.ErrGatewayContractUnhealthy) { t.Fatalf("Complete() error = %v", err) }
    if got := server.Counts()["chat_posts"]; got != 0 { t.Fatalf("chat POSTs = %d, want 0", got) }
}

func TestHealthyGatewayContractDoesNotBypassAttemptReceipts(t *testing.T) {
    server := newContractMockServer(t, contractMockConfig{})
    defer server.Close()
    config := testConfig(server.URL())
    config.RouteSafety = provider.SafeRouteSafety()
    client, err := New(config, Options{HTTPClient: server.Client()})
    if err != nil { t.Fatal(err) }
    if health := client.ProbeGatewayContract(context.Background()); !health.Healthy() { t.Fatalf("health = %#v", health) }
    _, err = client.Complete(context.Background(), provider.Request{Prompt: "synthetic"})
    if !errors.Is(err, provider.ErrUnsafeRoute) { t.Fatalf("Complete() error = %v, want unsafe route", err) }
    if got := server.Counts()["chat_posts"]; got != 0 { t.Fatalf("chat POSTs = %d, want 0", got) }
}
```

The helper `newReceiptAwareHealthTestClient(t, management)` must construct the existing contract mock, set `EnableAttemptReceipts: true`, `Provider: "chatgpt-web"`, `AccountLaneHash: "synthetic-lane-hash"`, and `RouteSafety: provider.ReceiptRouteSafety()`, then return the client and mock server.

Assert the existing `RouteSafety` value remains independent and that a healthy probe does not make `client.RouteSafety().Validate()` authorize protected execution.

- [ ] **Step 2: Run the direct-execution RED tests**

Run: `go test ./internal/provider/omniroute -run 'Test(CompleteBlocks|HealthyGatewayContract)' -count=1`
Expected: FAIL because `Complete` does not yet inspect gateway health.

- [ ] **Step 3: Add the direct adapter gate**

At the start of `Client.Complete`, after the nil check and before any completion/receipt work that could reach HTTP, read `GatewayContractHealth()`. Return `provider.ErrGatewayContractUnhealthy` whenever `Healthy()` is false. Keep the existing receipt gate after it, so a healthy result still requires authoritative receipts.

- [ ] **Step 4: Add RED governor tests with a provider-neutral fake**

Define a fake in the governor test package implementing only `provider.Client`, `provider.SafetyAware`, `provider.AttemptReceiptAware`, and `provider.ContractHealthAware`. Add cases for all three blocking states:

```go
func TestExecuteBlocksNonHealthyGatewayContractBeforeComplete(t *testing.T) {
    for _, state := range []provider.GatewayContractHealth{
        provider.GatewayContractHealthUnknown,
        provider.GatewayContractHealthDegraded,
        provider.GatewayContractHealthProtocolChanged,
    } {
        t.Run(state.String(), func(t *testing.T) {
            client := &healthAwareFakeClient{health: provider.GatewayContractHealthResult{State: state}}
            governor := receiptRequiredGovernor(t)
            result := governor.Execute(context.Background(), provider.AttemptRequest{
                TaskID: "task-1", ClientRequestID: "request-1",
                ProviderRequest: provider.Request{Model: "instant", Prompt: "synthetic"},
            }, client, nil)
            if result.Admission.Code != policy.AdmissionGatewayContractUnhealthy { t.Fatalf("admission = %#v", result.Admission) }
            if client.completeCalls != 0 { t.Fatalf("Complete calls = %d, want 0", client.completeCalls) }
        })
    }
}

func TestExecuteHealthyGatewayContractStillRequiresReceipts(t *testing.T) {
    client := &healthAwareFakeClient{health: provider.GatewayContractHealthResult{State: provider.GatewayContractHealthHealthy}}
    client.receipts = false
    result := receiptRequiredGovernor(t).Execute(context.Background(), provider.AttemptRequest{
        TaskID: "task-1", ClientRequestID: "request-1",
        ProviderRequest: provider.Request{Model: "instant", Prompt: "synthetic"},
    }, client, nil)
    if result.Admission.Code != policy.AdmissionMissingAttemptReceipts { t.Fatalf("admission = %#v", result.Admission) }
}
```

The governor config must use `provider.ReceiptRouteSafety()` and `RequireAttemptReceipts: true`; do not import `internal/provider/omniroute` in these tests.

- [ ] **Step 5: Run the governor RED tests**

Run: `go test ./internal/governor -run 'TestExecute.*GatewayContract' -count=1`
Expected: FAIL because `Governor.Execute` does not yet inspect the optional health capability.

- [ ] **Step 6: Add the provider-neutral governor gate**

In `Governor.Execute`, after the existing `SafetyAware` RouteSafety comparison and before the receipt capability/`Admit` path:

```go
if healthAware, ok := client.(provider.ContractHealthAware); ok {
    health := healthAware.GatewayContractHealth()
    if !health.Healthy() {
        result := g.gatewayContractHealthAdmission(request, health)
        return ExecutionResult{Admission: result, Err: result.Err}
    }
}
```

Implement `gatewayContractHealthAdmission` under the governor lock. It must produce `AdmissionGatewayContractUnhealthy`, preserve the typed result on `AdmissionResult`, enqueue the existing `EventAdmission`, and return an `AdmissionError` wrapping `provider.ErrGatewayContractUnhealthy`. It must not reserve, start, persist, or finish an attempt.

Providers without `ContractHealthAware` must follow the pre-existing path unchanged.

- [ ] **Step 7: Run direct and governor tests and verify green**

Run: `go test ./internal/provider/omniroute ./internal/governor -run 'Test(CompleteBlocks|HealthyGatewayContract|Execute.*GatewayContract)' -count=1`
Expected: PASS; non-healthy states produce no completion request and healthy never bypasses receipts.

- [ ] **Step 8: Commit the execution gates**

```bash
git add internal/provider/omniroute/client.go internal/provider/omniroute/gateway_contract_health_test.go internal/governor/execute.go internal/governor/gateway_contract_health_test.go internal/governor/admission.go
git commit -m "feat(governor): fail closed on gateway contract health"
```

---

### Task 4: Expose sanitized health in the existing trace surface

**Files:**
- Modify: `internal/trace/policy.go`
- Modify: `internal/trace/policy_test.go`
- Test: `internal/governor/gateway_contract_health_test.go`

**Interfaces:**
- Consumes the optional `governor.Event.GatewayContractHealth` result.
- Produces a structured `gateway_contract_health` group with only state, fixed reason code, fixed endpoint identifier, and timestamp.

- [ ] **Step 1: Add the failing trace assertion**

Extend `TestPolicySinkEmitsSanitizedStructuredEvent` or add a focused case:

```go
health := provider.GatewayContractHealthResult{
    State: provider.GatewayContractHealthProtocolChanged,
    ReasonCode: "missing_or_invalid_field",
    Endpoint: "/api/settings",
    CheckedAt: time.Date(2026, time.January, 1, 13, 0, 0, 0, time.UTC),
}
sink.Emit(governor.Event{Kind: governor.EventAdmission, GatewayContractHealth: &health})
// Decode JSON and assert record["gateway_contract_health"].state == "protocol_changed".
// Assert output does not contain "upstream_health", "chatgpt_health", raw body, or secret text.
```

- [ ] **Step 2: Run the trace RED test**

Run: `go test ./internal/trace -run 'TestPolicySink.*Health|TestPolicySinkEmitsSanitized' -count=1`
Expected: FAIL because the sink does not render the health group.

- [ ] **Step 3: Render the exact diagnostic name through the existing sink**

In `PolicySink.Emit`, build the existing `Info` argument slice and append this sanitized group only when the event carries a result:

```go
fields := []any{
    "kind", event.Kind,
    "account_policy_id", event.AccountPolicyID,
    "provider", event.ProviderID,
    "model_pool", event.ModelPool,
    "model", event.Model,
    "allowance_profile", event.AllowanceProfile,
    "task_id", event.TaskID,
    "client_request_id", event.ClientRequestID,
    "attempt_sequence", event.AttemptSequence,
    "admission", event.Admission,
    "reason", event.Reason,
    "delay", event.Delay,
    "retry_at", event.RetryAt,
    "outcome", event.Outcome,
    "cooldown_until", event.CooldownUntil,
    "selected_backoff", event.SelectedBackoff,
    "circuit_from", event.CircuitFrom,
    "circuit_to", event.CircuitTo,
    "circuit_reason", event.CircuitReason,
    "telemetry_healthy", event.TelemetryHealthy,
    slog.Group("telemetry",
        "available", event.Telemetry.Available,
        "remaining", telemetryRemaining(event.Telemetry.Remaining),
        "reset_at", event.Telemetry.ResetAt,
        "cooldown_until", event.Telemetry.CooldownUntil,
        "rate_limited", event.Telemetry.RateLimited,
        "capacity_exhausted", event.Telemetry.CapacityExhausted,
        "upstream_circuit", event.Telemetry.UpstreamCircuit,
    ),
    slog.Group("budgets_before",
        "rolling_3h_used", event.BudgetsBefore.Rolling3hUsed,
        "rolling_1h_used", event.BudgetsBefore.Rolling1hUsed,
        "rolling_10m_used", event.BudgetsBefore.Rolling10mUsed,
        "task_used", event.BudgetsBefore.TaskUsed,
        "retries_used", event.BudgetsBefore.RetriesUsed,
        "manual_reserve_remaining", event.BudgetsBefore.ManualReserveRemaining,
    ),
    slog.Group("budgets_after",
        "rolling_3h_used", event.BudgetsAfter.Rolling3hUsed,
        "rolling_1h_used", event.BudgetsAfter.Rolling1hUsed,
        "rolling_10m_used", event.BudgetsAfter.Rolling10mUsed,
        "task_used", event.BudgetsAfter.TaskUsed,
        "retries_used", event.BudgetsAfter.RetriesUsed,
        "manual_reserve_remaining", event.BudgetsAfter.ManualReserveRemaining,
    ),
}
if event.GatewayContractHealth != nil {
    fields = append(fields, slog.Group("gateway_contract_health",
        "state", event.GatewayContractHealth.State.String(),
        "reason_code", event.GatewayContractHealth.ReasonCode,
        "endpoint", event.GatewayContractHealth.Endpoint,
        "checked_at", event.GatewayContractHealth.CheckedAt,
    ))
}
s.logger.Info("account policy event", fields...)
```

Preserve every existing field and telemetry group in `fields`; the snippet shows only the insertion point. Use the existing structured logger and do not add a new sink, logger, goroutine, or persisted telemetry field. Keep the reason and endpoint values sanitized by the adapter before they enter the event.

- [ ] **Step 4: Run trace tests and verify green**

Run: `go test ./internal/trace -run 'TestPolicySink.*Health|TestPolicySinkEmitsSanitized' -count=1`
Expected: PASS with `gateway_contract_health` and no secret/raw-body fields.

- [ ] **Step 5: Commit the trace integration**

```bash
git add internal/trace/policy.go internal/trace/policy_test.go
git commit -m "feat(trace): render gateway contract health"
```

---

### Task 5: Documentation, fixture hygiene, and full validation

**Files:**
- Modify: `docs/account-protection.md` with one concise gateway-contract health distinction section.
- Existing: `docs/superpowers/specs/2026-08-13-gateway-contract-health-design.md`
- Existing: all issue #43 fixture/mock files

- [ ] **Step 1: Add the concise user-facing distinction**

Document that `gateway_contract_health` is an on-demand management-contract signal, not ChatGPT Web/upstream health and not authoritative attempt accounting. Do not document a live activation, scheduler, retries, fallback, or receipt replacement.

- [ ] **Step 2: Run focused provider/governor/trace tests**

```bash
go test -count=1 ./internal/provider/...
go test -race -count=1 ./internal/provider/...
go test -count=1 ./internal/governor/...
go test -race -count=1 ./internal/governor/...
go test ./internal/trace/... -count=1
```

Expected: all packages PASS.

- [ ] **Step 3: Run the mandatory repository validation commands**

```bash
test -z "$(gofmt -l .)"
go test ./...
go vet ./...
go build ./cmd/runstead
go test -race ./...
bash experiments/protocol/test.sh
git diff --check
```

Record the actual exit status and relevant package counts. If any command fails, fix the cause, rerun the failed command, and rerun the complete validation set before proceeding.

- [ ] **Step 4: Review the complete diff against origin/main**

```bash
git diff --stat origin/main...HEAD
git diff --check origin/main...HEAD
git diff origin/main...HEAD -- internal/provider internal/governor internal/trace docs
```

Confirm manually that the diff contains no model POST in the probe, no retry, no new dependency, no unrelated files, no upstream-health naming, no receipt bypass, no governor-to-OmniRoute import, no raw secrets/bodies, no M6/browser work, and only synthetic fixtures.

- [ ] **Step 5: Commit documentation if it changed and verify the final worktree**

```bash
git status --short
git log --oneline --decorate origin/main..HEAD
git diff --check origin/main...HEAD
```

Commit only the concise documentation change if required:

```bash
git add docs/account-protection.md
git commit -m "docs(provider): distinguish gateway contract health"
```

- [ ] **Step 6: Request review, push, and open exactly one PR**

Run a final code review against `origin/main` and the branch tip. Then:

```bash
git push -u origin feat/issue-40-gateway-contract-health
gh pr create --base main --head feat/issue-40-gateway-contract-health \
  --title "feat(provider): add OmniRoute gateway-contract health probe" \
  --body-file <(cat <<'EOF'
Closes #40

## Summary
- Added a typed, on-demand OmniRoute gateway-contract health model and bounded GET-only probe for `/api/providers`, `/api/settings`, and `/api/models/alias`.
- Kept unknown, degraded, ambiguous, and protocol-changed states fail-closed for protected execution.
- Integrated health as an additional provider-neutral governor/direct-adapter gate without changing RouteSafety or receipt/accounting requirements.
- Reused the issue #43 synthetic contract corpus and mock; no live ChatGPT Web or upstream health claim is made.
- Exposed the latest sanitized result as `gateway_contract_health` through the existing trace/event surface.

## Validation
List only commands actually executed and their real results.
EOF
)
```

Do not merge the PR or start another issue.
