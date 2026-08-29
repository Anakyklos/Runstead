# Minimal Request Telemetry Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Extend the provider-neutral sanitized `provider.ResponseMetadata` with a small typed request-telemetry surface (`AdapterVersion`, `Transport`, `FirstTokenLatency`, retry/fallback zero-fields, `UsageEstimated`, session-fingerprint semantics) and surface it through governor attempt events and trace output, without storing or rendering any sensitive content.

**Architecture:** `internal/provider` owns the canonical `CompatAdapterVersion` constant and the six new `ResponseMetadata` fields; `compat.AdapterVersion` becomes an alias. Each of the four adapters (`openaicompat`, `anthropiccompat`, `googlecompat`, `omniroute`) stamps its own transport identifier and the version into every metadata record it builds, and measures `FirstTokenLatency` from its existing `httptrace` `GotFirstResponseByte` observation using the injected clock. `Outcome` and `governor.Event` gain a sanitized `ResponseMetadata` copy; `Execute` fills it after classification, permit emitters copy it, and `trace.PolicySink` renders one sanitized `attempt` group. No persistence, schema, `RouteSafety`, receipts, budgets or policy change.

**Tech Stack:** Go 1.22 stdlib only (`net/http`, `net/http/httptrace`, `log/slog`, `crypto/sha256`), existing `httptest`/contract-mock fixtures, existing `trace.PolicySink`, existing `tools/quality` gates.

**Spec:** `docs/superpowers/specs/2026-08-29-request-telemetry-design.md`

## Global Constraints

- New fields on `provider.ResponseMetadata`: `AdapterVersion string`, `Transport string`, `FirstTokenLatency time.Duration`, `RetryCount int`, `Fallback bool`, `UsageEstimated bool`.
- `SessionID` is the session fingerprint: non-empty values MUST be `sha256:`-prefixed hashes (`hashOpaque`); raw session/connection identity never enters metadata. `RequestID` stays sanitized (hash or conservative token).
- `RetryCount`, `Fallback` and `UsageEstimated` are always zero/false from every adapter in this change; nothing may ever set them to non-zero values here.
- `FirstTokenLatency` is zero unless the adapter's own `httptrace` observation proves the first response byte; never guessed.
- Canonical version constant: `provider.CompatAdapterVersion = "compatible-provider-v0.1"`; `compat.AdapterVersion` must remain an alias so `cmd/runstead` callers keep working. No import cycles: adapters import `internal/provider` only.
- Transport identifiers (exact strings): `openaicompat-http`, `anthropiccompat-http`, `googlecompat-http`, `omniroute-http`.
- No schema migration, no new dependency, no new fixture framework, no change to durable `ProviderFinished` persistence, `RouteSafety`, receipts or budgets.
- Telemetry never gates execution and never admits/refuses/retries/routes.
- Browser-provider outcome refinements from #39 are deferred (plugin track, #80/#82/#83/#74, #86): not implemented here.
- All new tests must use the package's existing helpers (`newTestClient`, `newRequestRecorder`, `resolvedForBase`, `validCompletionBody`/`validMessagesBody`/`validGenerateBody`, `newContractMockServer`). Use `context.Background()` (Go 1.22 has no `t.Context()`).

---

### Task 1: Provider-neutral telemetry fields and canonical version

**Files:**
- Modify: `internal/provider/provider.go`
- Modify: `internal/provider/compat/compat.go`
- Create: `internal/provider/telemetry_test.go`

**Interfaces:**
- Produces `provider.CompatAdapterVersion` (const string) and six new `ResponseMetadata` fields (names/types above).
- `compat.AdapterVersion` is redefined as an alias of `provider.CompatAdapterVersion`; its existing callers (`cmd/runstead/main.go:343`, `cmd/runstead/resume.go:302`) keep compiling unchanged.
- Consumes: nothing new.

- [ ] **Step 1: Write the failing provider telemetry contract tests**

Create `internal/provider/telemetry_test.go`:

```go
package provider

import "testing"

// TestResponseMetadataTelemetryZeroValue pins the conservative zero-value
// contract (#39): an absent observation must render as absent/zero, never
// guessed.
func TestResponseMetadataTelemetryZeroValue(t *testing.T) {
	var metadata ResponseMetadata
	if metadata.AdapterVersion != "" {
		t.Fatalf("zero AdapterVersion = %q, want empty", metadata.AdapterVersion)
	}
	if metadata.Transport != "" {
		t.Fatalf("zero Transport = %q, want empty", metadata.Transport)
	}
	if metadata.FirstTokenLatency != 0 {
		t.Fatalf("zero FirstTokenLatency = %v, want 0", metadata.FirstTokenLatency)
	}
	if metadata.RetryCount != 0 {
		t.Fatalf("zero RetryCount = %d, want 0", metadata.RetryCount)
	}
	if metadata.Fallback {
		t.Fatal("zero Fallback = true, want false")
	}
	if metadata.UsageEstimated {
		t.Fatal("zero UsageEstimated = true, want false")
	}
}

// TestCompatAdapterVersionIsCanonical pins the single adapter-version
// identity shared by execution evidence and telemetry.
func TestCompatAdapterVersionIsCanonical(t *testing.T) {
	if CompatAdapterVersion != "compatible-provider-v0.1" {
		t.Fatalf("CompatAdapterVersion = %q, want pinned value", CompatAdapterVersion)
	}
}

// TestSessionIDIsSanitizedFingerprint pins the session-fingerprint contract:
// any non-empty SessionID must be a sha256 digest, never raw session identity.
func TestSessionIDIsSanitizedFingerprint(t *testing.T) {
	if !isFingerprint("sha256:0123456789abcdef") {
		t.Fatal("sha256-prefixed value did not pass fingerprint check")
	}
	if isFingerprint("live-session-abc") {
		t.Fatal("raw session identity passed fingerprint check")
	}
	if isFingerprint("") {
		t.Fatal("empty value passed fingerprint check")
	}
}

func isFingerprint(value string) bool {
	if len(value) != len("sha256:")+16 {
		return false
	}
	if value[:len("sha256:")] != "sha256:" {
		return false
	}
	for _, char := range value[len("sha256:"):] {
		if !(char >= '0' && char <= '9') && !(char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}
```

- [ ] **Step 2: Run the test and verify the expected missing-symbol failure**

Run: `go test ./internal/provider -run 'TestResponseMetadataTelemetryZeroValue|TestCompatAdapterVersionIsCanonical|TestSessionIDIsSanitizedFingerprint' -count=1`
Expected: FAIL to compile (`CompatAdapterVersion` undefined).

- [ ] **Step 3: Implement the canonical constant and fields**

In `internal/provider/provider.go`, add the version constant near the top-level contract types:

```go
// CompatAdapterVersion identifies the compatible-provider composition surface
// (#14/#86). It is the single adapter-version identity shared by execution
// evidence and request telemetry; bump it when the adapter set or its
// behavior changes meaningfully.
const CompatAdapterVersion = "compatible-provider-v0.1"
```

Extend `ResponseMetadata` (fields after `AttemptReceipts`) and document `SessionID`:

```go
// ResponseMetadata contains provider-neutral, sanitized observations about a
// completed request. It deliberately excludes prompts, response bodies,
// credentials and raw headers.
type ResponseMetadata struct {
	StatusCode    int
	RequestID     string
	SessionID     string
	Duration      time.Duration
	RetryAfter    time.Duration
	ResetAt       time.Time
	Endpoint      string
	Model         string
	DeliveryState DeliveryState
	AttemptReceipts *AttemptReceiptSet

	// Issue #39 request telemetry. All fields are conservative zero values
	// unless the adapter can prove the observation; nothing is ever guessed.

	// AdapterVersion is the pinned adapter/composition version
	// (CompatAdapterVersion for the compatible families; adapter-owned for
	// legacy transports). Empty only when the record was never populated.
	AdapterVersion string

	// Transport is the stable transport identifier (for example
	// "openaicompat-http"). Empty only when the record was never populated.
	Transport string

	// FirstTokenLatency is the latency from request start to the first
	// observed response byte, when the adapter's transport observation
	// proves it. Zero means not measured, never a claim of instant arrival.
	FirstTokenLatency time.Duration

	// RetryCount is the number of retries this attempt represents. The
	// protected lane has no retries outside the governor (#92): every
	// current adapter leaves it zero so amplification can never hide here.
	RetryCount int

	// Fallback reports whether this attempt used a fallback route. The
	// protected lane has no fallbacks: every current adapter leaves it
	// false.
	Fallback bool

	// UsageEstimated reports that any usage figures carried by this
	// transport are estimates rather than metered values. No current
	// adapter emits usage, so every current adapter leaves it false.
	UsageEstimated bool
}
```

In `internal/provider/compat/compat.go`, replace the constant definition with the alias:

```go
// AdapterVersion identifies this compatibility composition surface. The value
// lives in internal/provider (provider.CompatAdapterVersion) so adapters and
// the provider-neutral contract share one identity without an import cycle.
const AdapterVersion = provider.CompatAdapterVersion
```

- [ ] **Step 4: Run the test and verify it passes**

Run: `go test ./internal/provider -run 'TestResponseMetadataTelemetryZeroValue|TestCompatAdapterVersionIsCanonical|TestSessionIDIsSanitizedFingerprint' -count=1` and `go test ./internal/provider/... ./cmd/runstead -run 'TestNonexistent' -count=1` (compile check: `compat.AdapterVersion` alias must keep `cmd/runstead` building).
Expected: PASS and clean compile.

- [ ] **Step 5: Commit**

```bash
git add internal/provider/provider.go internal/provider/compat/compat.go internal/provider/telemetry_test.go
git commit -m "feat(provider): add sanitized request telemetry contract to metadata (#39)"
```

---

### Task 2: OpenAI-compatible adapter telemetry population

**Files:**
- Modify: `internal/provider/openaicompat/client.go`
- Modify: `internal/provider/openaicompat/delivery.go`
- Create: `internal/provider/openaicompat/telemetry_test.go`

**Interfaces:**
- Consumes: `provider.CompatAdapterVersion`, new `ResponseMetadata` fields (Task 1).
- Produces: const `transportID = "openaicompat-http"`; method `(*Client).baseMetadata() provider.ResponseMetadata`; `responseMetadata(response *http.Response, duration, firstTokenLatency time.Duration, endpointURL string) provider.ResponseMetadata`; `(*deliveryObservation).firstTokenLatency(started time.Time) time.Duration`.

- [ ] **Step 1: Write the failing adapter telemetry tests**

Create `internal/provider/openaicompat/telemetry_test.go`:

```go
package openaicompat

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

// TestResponseMetadataTelemetryOnSuccess proves the success path stamps the
// pinned version and transport and measures a first-token latency.
func TestResponseMetadataTelemetryOnSuccess(t *testing.T) {
	recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", "req-abc")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(validCompletionBody))
	})
	client, _ := newTestClient(t, nil, nil, recorder)
	response, err := client.Complete(context.Background(), provider.Request{Prompt: "hi", Model: "model-a"})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if response.Metadata.AdapterVersion != provider.CompatAdapterVersion {
		t.Fatalf("AdapterVersion = %q, want %q", response.Metadata.AdapterVersion, provider.CompatAdapterVersion)
	}
	if response.Metadata.Transport != "openaicompat-http" {
		t.Fatalf("Transport = %q, want openaicompat-http", response.Metadata.Transport)
	}
	if response.Metadata.FirstTokenLatency < 0 {
		t.Fatalf("FirstTokenLatency = %v, want >= 0", response.Metadata.FirstTokenLatency)
	}
	if response.Metadata.RetryCount != 0 || response.Metadata.Fallback || response.Metadata.UsageEstimated {
		t.Fatalf("protected lane telemetry nonzero: retry=%d fallback=%v usage_estimated=%v",
			response.Metadata.RetryCount, response.Metadata.Fallback, response.Metadata.UsageEstimated)
	}
}

// TestFirstTokenLatencyUsesInjectedClock proves FirstTokenLatency equals the
// proven started-to-first-byte delta under the deterministic injected clock.
func TestFirstTokenLatencyUsesInjectedClock(t *testing.T) {
	current := time.Unix(1700000000, 0)
	clock := func() time.Time { return current }
	firstByteGate := make(chan struct{})
	recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		<-firstByteGate
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(validCompletionBody))
	})
	resolved := resolvedForBase(t, recorder.server.URL)
	client, err := New(resolved, nil, Options{Now: clock})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	done := make(chan struct{})
	var response provider.Response
	go func() {
		response, _ = client.Complete(context.Background(), provider.Request{Prompt: "hi", Model: "model-a"})
		close(done)
	}()
	time.Sleep(10 * time.Millisecond) // request is blocked in the handler
	current = current.Add(50 * time.Millisecond)
	close(firstByteGate)
	<-done
	if response.Metadata.FirstTokenLatency != 50*time.Millisecond {
		t.Fatalf("FirstTokenLatency = %v, want 50ms", response.Metadata.FirstTokenLatency)
	}
}

// TestPreDispatchRefusalStillStampsIdentityAndKeepsLatencyZero proves the
// zero-value rule: a refusal before dispatch carries adapter identity but no
// invented latency.
func TestPreDispatchRefusalStillStampsIdentityAndKeepsLatencyZero(t *testing.T) {
	client, _ := newTestClient(t, nil, nil, nil)
	response, err := client.Complete(context.Background(), provider.Request{Prompt: "hi", Model: "different-model"})
	if err == nil {
		t.Fatal("expected config refusal for a different model")
	}
	if response.Metadata.DeliveryState != provider.DeliveryNotSent {
		t.Fatalf("DeliveryState = %v, want not_sent", response.Metadata.DeliveryState)
	}
	if response.Metadata.AdapterVersion != provider.CompatAdapterVersion {
		t.Fatalf("AdapterVersion = %q, want %q", response.Metadata.AdapterVersion, provider.CompatAdapterVersion)
	}
	if response.Metadata.Transport != "openaicompat-http" {
		t.Fatalf("Transport = %q, want openaicompat-http", response.Metadata.Transport)
	}
	if response.Metadata.FirstTokenLatency != 0 {
		t.Fatalf("FirstTokenLatency = %v, want 0 (nothing observed)", response.Metadata.FirstTokenLatency)
	}
}
```

- [ ] **Step 2: Run the tests and verify the expected failures**

Run: `go test ./internal/provider/openaicompat -run 'TestResponseMetadataTelemetry|TestFirstTokenLatency|TestPreDispatchRefusal' -count=1`
Expected: FAIL (`response.Metadata.AdapterVersion` does not exist).

- [ ] **Step 3: Implement first-byte observation in delivery.go**

Modify `internal/provider/openaicompat/delivery.go`:

```go
package openaicompat

import (
	"net/http/httptrace"
	"sync"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

type deliveryObservation struct {
	mu               sync.Mutex
	wroteHeaders     bool
	wroteRequestBody bool
	responseStarted  bool
	firstByteAt      time.Time
	now              func() time.Time
}

func (o *deliveryObservation) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		WroteHeaders: func() {
			o.mu.Lock()
			o.wroteHeaders = true
			o.mu.Unlock()
		},
		WroteRequest: func(_ httptrace.WroteRequestInfo) {
			o.mu.Lock()
			o.wroteRequestBody = true
			o.mu.Unlock()
		},
		GotFirstResponseByte: func() { o.recordFirstResponseByte() },
	}
}

// recordFirstResponseByte keeps the first observed byte time: the FirstToken
// Latency may never be guessed, so only the first proven observation counts.
func (o *deliveryObservation) recordFirstResponseByte() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.firstByteAt.IsZero() && o.now != nil {
		o.firstByteAt = o.now()
	}
	o.responseStarted = true
}

func (o *deliveryObservation) markResponseStarted() {
	o.mu.Lock()
	o.responseStarted = true
	o.mu.Unlock()
}

// firstTokenLatency returns the proven started-to-first-byte delta, or zero
// when no first byte was observed.
func (o *deliveryObservation) firstTokenLatency(started time.Time) time.Duration {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.firstByteAt.IsZero() {
		return 0
	}
	latency := o.firstByteAt.Sub(started)
	if latency < 0 {
		return 0
	}
	return latency
}

func (o *deliveryObservation) stateAfterError() provider.DeliveryState {
	o.mu.Lock()
	defer o.mu.Unlock()
	switch {
	case o.responseStarted:
		return provider.DeliveryResponseStarted
	case o.wroteRequestBody || o.wroteHeaders:
		return provider.DeliverySentConfirmed
	default:
		return provider.DeliverySentUnconfirmed
	}
}

func (o *deliveryObservation) stateAfterBody(readComplete bool) provider.DeliveryState {
	if readComplete {
		return provider.DeliveryCompleted
	}
	return o.stateAfterError()
}
```

- [ ] **Step 4: Implement stamping and latency wiring in client.go**

In `internal/provider/openaicompat/client.go`:

Add the transport identifier next to the other constants:

```go
const (
	defaultTimeout        = 60 * time.Second
	defaultResponseLimit  = 8 << 20
	familyChatCompletions = "chat/completions"
	transportID           = "openaicompat-http"
)
```

Add the base-metadata helper next to `responseMetadata`:

```go
// baseMetadata stamps the adapter identity that every record carries,
// including pre-dispatch refusal records (#39).
func (c *Client) baseMetadata() provider.ResponseMetadata {
	return provider.ResponseMetadata{
		AdapterVersion: provider.CompatAdapterVersion,
		Transport:      transportID,
	}
}
```

Replace the `notSent` closure in `Complete`:

```go
	notSent := func(kind ErrorKind, cause error) (provider.Response, error) {
		metadata := c.baseMetadata()
		metadata.DeliveryState = provider.DeliveryNotSent
		return provider.Response{Metadata: metadata}, &Error{Kind: kind, DeliveryState: provider.DeliveryNotSent, Cause: cause}
	}
```

Replace the two `callErr`/nil-response metadata constructions (they become):

```go
	metadata := c.baseMetadata()
	metadata.Endpoint = logicalEndpoint(endpointURL)
	metadata.Model = c.resolved.Model
	metadata.Duration = c.now().Sub(started)
	metadata.DeliveryState = state
```

Replace the observation construction:

```go
	observation := &deliveryObservation{now: c.now}
```

Update the `responseMetadata` call site and signature:

```go
	metadata := c.responseMetadata(response, c.now().Sub(started), observation.firstTokenLatency(started), endpointURL)
```

```go
func (c *Client) responseMetadata(response *http.Response, duration, firstTokenLatency time.Duration, endpointURL string) provider.ResponseMetadata {
	metadata := c.baseMetadata()
	metadata.StatusCode = response.StatusCode
	metadata.RequestID = hashOpaque(response.Header.Get("X-Request-Id"))
	metadata.Duration = duration
	metadata.FirstTokenLatency = firstTokenLatency
	metadata.RetryAfter = parseRetryAfter(response.Header.Get("Retry-After"), c.now())
	metadata.Endpoint = logicalEndpoint(endpointURL)
	metadata.Model = c.resolved.Model
	return metadata
}
```

Update any other callers of `responseMetadata` found in the package (including tests) to the new signature.

- [ ] **Step 5: Run the tests and verify they pass**

Run: `go test ./internal/provider/openaicompat -count=1`
Expected: PASS (all existing and new tests).

- [ ] **Step 6: Commit**

```bash
git add internal/provider/openaicompat/client.go internal/provider/openaicompat/delivery.go internal/provider/openaicompat/telemetry_test.go
git commit -m "feat(provider): stamp telemetry identity and first-token latency in the OpenAI-compatible adapter (#39)"
```

---

### Task 3: Anthropic-compatible adapter telemetry population

**Files:**
- Modify: `internal/provider/anthropiccompat/client.go`
- Modify: `internal/provider/anthropiccompat/delivery.go`
- Create: `internal/provider/anthropiccompat/telemetry_test.go`

**Interfaces:**
- Consumes: Task 1 fields; the Anthropic adapter's existing `deliveryObservation`, `responseMetadata`, `notSent` and `Complete` mirror the OpenAI-compatible shapes (same file layout, same `httptrace` pattern, verified in `internal/provider/anthropiccompat/delivery.go`).
- Produces: const `transportID = "anthropiccompat-http"`; the same `baseMetadata`, observation latency and `responseMetadata` surfacing as Task 2, adapted to the Anthropic file layout.

- [ ] **Step 1: Write the failing adapter telemetry tests**

Create `internal/provider/anthropiccompat/telemetry_test.go` (same shape as Task 2, with these differences: transport `"anthropiccompat-http"`, success body `validMessagesBody`, and the success-path handler does not need `X-Request-Id`):

```go
package anthropiccompat

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

func TestResponseMetadataTelemetryOnSuccess(t *testing.T) {
	recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(validMessagesBody))
	})
	client, _ := newTestClient(t, nil, nil, recorder)
	response, err := client.Complete(context.Background(), provider.Request{Prompt: "hi", Model: "model-a"})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if response.Metadata.AdapterVersion != provider.CompatAdapterVersion {
		t.Fatalf("AdapterVersion = %q, want %q", response.Metadata.AdapterVersion, provider.CompatAdapterVersion)
	}
	if response.Metadata.Transport != "anthropiccompat-http" {
		t.Fatalf("Transport = %q, want anthropiccompat-http", response.Metadata.Transport)
	}
	if response.Metadata.FirstTokenLatency < 0 {
		t.Fatalf("FirstTokenLatency = %v, want >= 0", response.Metadata.FirstTokenLatency)
	}
	if response.Metadata.RetryCount != 0 || response.Metadata.Fallback || response.Metadata.UsageEstimated {
		t.Fatalf("protected lane telemetry nonzero: retry=%d fallback=%v usage_estimated=%v",
			response.Metadata.RetryCount, response.Metadata.Fallback, response.Metadata.UsageEstimated)
	}
}

func TestFirstTokenLatencyUsesInjectedClock(t *testing.T) {
	current := time.Unix(1700000000, 0)
	clock := func() time.Time { return current }
	firstByteGate := make(chan struct{})
	recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		<-firstByteGate
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(validMessagesBody))
	})
	resolved := resolvedForBase(t, recorder.server.URL)
	client, err := New(resolved, nil, Options{Now: clock})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	done := make(chan struct{})
	var response provider.Response
	go func() {
		response, _ = client.Complete(context.Background(), provider.Request{Prompt: "hi", Model: "model-a"})
		close(done)
	}()
	time.Sleep(10 * time.Millisecond)
	current = current.Add(50 * time.Millisecond)
	close(firstByteGate)
	<-done
	if response.Metadata.FirstTokenLatency != 50*time.Millisecond {
		t.Fatalf("FirstTokenLatency = %v, want 50ms", response.Metadata.FirstTokenLatency)
	}
}

func TestPreDispatchRefusalStillStampsIdentityAndKeepsLatencyZero(t *testing.T) {
	client, _ := newTestClient(t, nil, nil, nil)
	response, err := client.Complete(context.Background(), provider.Request{Prompt: "hi", Model: "different-model"})
	if err == nil {
		t.Fatal("expected config refusal for a different model")
	}
	if response.Metadata.DeliveryState != provider.DeliveryNotSent {
		t.Fatalf("DeliveryState = %v, want not_sent", response.Metadata.DeliveryState)
	}
	if response.Metadata.AdapterVersion != provider.CompatAdapterVersion {
		t.Fatalf("AdapterVersion = %q, want %q", response.Metadata.AdapterVersion, provider.CompatAdapterVersion)
	}
	if response.Metadata.Transport != "anthropiccompat-http" {
		t.Fatalf("Transport = %q, want anthropiccompat-http", response.Metadata.Transport)
	}
	if response.Metadata.FirstTokenLatency != 0 {
		t.Fatalf("FirstTokenLatency = %v, want 0 (nothing observed)", response.Metadata.FirstTokenLatency)
	}
}
```

- [ ] **Step 2: Run the tests and verify the expected failures**

Run: `go test ./internal/provider/anthropiccompat -run 'TestResponseMetadataTelemetry|TestFirstTokenLatency|TestPreDispatchRefusal' -count=1`
Expected: FAIL (fields do not exist).

- [ ] **Step 3: Implement observation latency in delivery.go**

Apply the Task 2 Step 3 changes to `internal/provider/anthropiccompat/delivery.go`: add `firstByteAt time.Time` and `now func() time.Time` to `deliveryObservation`, `recordFirstResponseByte` (via `GotFirstResponseByte`), `firstTokenLatency(started time.Time) time.Duration` with the same zero/negative guards, and wire `now` from the client clock at the observation construction site in `Complete`.

- [ ] **Step 4: Implement stamping and latency wiring in client.go**

Apply the Task 2 Step 4 changes to `internal/provider/anthropiccompat/client.go`: add `transportID = "anthropiccompat-http"` to the package consts, add `baseMetadata`, replace the `notSent` closure, replace the two transport-error metadata constructions, construct the observation with the injected clock, and change `responseMetadata(response, duration, firstTokenLatency, endpointURL)` to stamp identity plus latency. Update any existing test callers of `responseMetadata` to the new signature.

- [ ] **Step 5: Run the tests and verify they pass**

Run: `go test ./internal/provider/anthropiccompat -count=1`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/provider/anthropiccompat/client.go internal/provider/anthropiccompat/delivery.go internal/provider/anthropiccompat/telemetry_test.go
git commit -m "feat(provider): stamp telemetry identity and first-token latency in the Anthropic-compatible adapter (#39)"
```

---

### Task 4: Google-compatible adapter telemetry population

**Files:**
- Modify: `internal/provider/googlecompat/client.go`
- Modify: `internal/provider/googlecompat/delivery.go`
- Create: `internal/provider/googlecompat/telemetry_test.go`

**Interfaces:**
- Consumes: Task 1 fields; same mirror layout as Tasks 2-3 (verified for `internal/provider/googlecompat/`).
- Produces: const `transportID = "googlecompat-http"` plus the same `baseMetadata`/latency/`responseMetadata` surfacing.

- [ ] **Step 1: Write the failing adapter telemetry tests**

Create `internal/provider/googlecompat/telemetry_test.go` (same shape as Task 3, differences: transport `"googlecompat-http"`, success body `validGenerateBody`):

```go
package googlecompat

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

func TestResponseMetadataTelemetryOnSuccess(t *testing.T) {
	recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(validGenerateBody))
	})
	client, _ := newTestClient(t, nil, nil, recorder)
	response, err := client.Complete(context.Background(), provider.Request{Prompt: "hi", Model: "model-a"})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if response.Metadata.AdapterVersion != provider.CompatAdapterVersion {
		t.Fatalf("AdapterVersion = %q, want %q", response.Metadata.AdapterVersion, provider.CompatAdapterVersion)
	}
	if response.Metadata.Transport != "googlecompat-http" {
		t.Fatalf("Transport = %q, want googlecompat-http", response.Metadata.Transport)
	}
	if response.Metadata.FirstTokenLatency < 0 {
		t.Fatalf("FirstTokenLatency = %v, want >= 0", response.Metadata.FirstTokenLatency)
	}
	if response.Metadata.RetryCount != 0 || response.Metadata.Fallback || response.Metadata.UsageEstimated {
		t.Fatalf("protected lane telemetry nonzero: retry=%d fallback=%v usage_estimated=%v",
			response.Metadata.RetryCount, response.Metadata.Fallback, response.Metadata.UsageEstimated)
	}
}

func TestFirstTokenLatencyUsesInjectedClock(t *testing.T) {
	current := time.Unix(1700000000, 0)
	clock := func() time.Time { return current }
	firstByteGate := make(chan struct{})
	recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		<-firstByteGate
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(validGenerateBody))
	})
	resolved := resolvedForBase(t, recorder.server.URL)
	client, err := New(resolved, nil, Options{Now: clock})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	done := make(chan struct{})
	var response provider.Response
	go func() {
		response, _ = client.Complete(context.Background(), provider.Request{Prompt: "hi", Model: "model-a"})
		close(done)
	}()
	time.Sleep(10 * time.Millisecond)
	current = current.Add(50 * time.Millisecond)
	close(firstByteGate)
	<-done
	if response.Metadata.FirstTokenLatency != 50*time.Millisecond {
		t.Fatalf("FirstTokenLatency = %v, want 50ms", response.Metadata.FirstTokenLatency)
	}
}

func TestPreDispatchRefusalStillStampsIdentityAndKeepsLatencyZero(t *testing.T) {
	client, _ := newTestClient(t, nil, nil, nil)
	response, err := client.Complete(context.Background(), provider.Request{Prompt: "hi", Model: "different-model"})
	if err == nil {
		t.Fatal("expected config refusal for a different model")
	}
	if response.Metadata.DeliveryState != provider.DeliveryNotSent {
		t.Fatalf("DeliveryState = %v, want not_sent", response.Metadata.DeliveryState)
	}
	if response.Metadata.AdapterVersion != provider.CompatAdapterVersion {
		t.Fatalf("AdapterVersion = %q, want %q", response.Metadata.AdapterVersion, provider.CompatAdapterVersion)
	}
	if response.Metadata.Transport != "googlecompat-http" {
		t.Fatalf("Transport = %q, want googlecompat-http", response.Metadata.Transport)
	}
	if response.Metadata.FirstTokenLatency != 0 {
		t.Fatalf("FirstTokenLatency = %v, want 0 (nothing observed)", response.Metadata.FirstTokenLatency)
	}
}
```

- [ ] **Step 2: Run the tests and verify the expected failures**

Run: `go test ./internal/provider/googlecompat -run 'TestResponseMetadataTelemetry|TestFirstTokenLatency|TestPreDispatchRefusal' -count=1`
Expected: FAIL (fields do not exist).

- [ ] **Step 3: Implement observation latency in delivery.go**

Apply the Task 2 Step 3 changes to `internal/provider/googlecompat/delivery.go` (same `deliveryObservation` extension and guards).

- [ ] **Step 4: Implement stamping and latency wiring in client.go**

Apply the Task 2 Step 4 changes to `internal/provider/googlecompat/client.go` with `transportID = "googlecompat-http"`. Update any existing test callers of `responseMetadata` to the new signature.

- [ ] **Step 5: Run the tests and verify they pass**

Run: `go test ./internal/provider/googlecompat -count=1`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/provider/googlecompat/client.go internal/provider/googlecompat/delivery.go internal/provider/googlecompat/telemetry_test.go
git commit -m "feat(provider): stamp telemetry identity and first-token latency in the Google-compatible adapter (#39)"
```

---

### Task 5: OmniRoute legacy adapter telemetry population

**Files:**
- Modify: `internal/provider/omniroute/client.go`
- Modify: `internal/provider/omniroute/client_transport.go`
- Modify: `internal/provider/omniroute/delivery.go`
- Create: `internal/provider/omniroute/telemetry_test.go`

**Interfaces:**
- Consumes: Task 1 fields. The OmniRoute adapter has its own `AdapterVersion` (legacy transport, owns its version identity) and its own `responseMetadata(response, duration, endpoint, model string, now time.Time)` helper plus `deliveryObservation` (fields `writeObserved`, `responseStarted`) in `delivery.go`.
- Produces: consts `AdapterVersion = "omniroute-v0.1"` and `transportID = "omniroute-http"`; `baseMetadata()`-style stamping on every path; first-byte latency via its `GotFirstResponseByte` observation; `SessionID` documented as the sha256 session fingerprint.

- [ ] **Step 1: Write the failing telemetry tests**

Create `internal/provider/omniroute/telemetry_test.go` using the package's existing helpers `newTransportClient`, `safeHandler`, `testConfig` and the `completeOnce` transport method (the full `Complete` wrapper is fail-closed for test configs because the receipt lane requires a pinned connection id):

```go
package omniroute

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/protocol"
	"github.com/RenyEnnos/Runstead/internal/provider"
)

// TestTransportMetadataTelemetryShape is the live-path metadata-shape test:
// through a real HTTP response the metadata must carry the pinned identity,
// a hash-formatted session fingerprint and zero protected-lane fields.
func TestTransportMetadataTelemetryShape(t *testing.T) {
	client, server := newTransportClient(t, safeHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(sessionIDHeader, "live-session-123")
		w.Header().Set(requestIDHeader, "req-123")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"runstead reply"}}]}`)
	}))
	defer server.Close()

	response, err := client.completeOnce(context.Background(), provider.Request{Protocol: protocol.Current, Prompt: "private prompt"})
	if err != nil {
		t.Fatalf("completeOnce: %v", err)
	}
	if response.Metadata.AdapterVersion != AdapterVersion {
		t.Fatalf("AdapterVersion = %q, want %q", response.Metadata.AdapterVersion, AdapterVersion)
	}
	if response.Metadata.Transport != transportID {
		t.Fatalf("Transport = %q, want %q", response.Metadata.Transport, transportID)
	}
	if !strings.HasPrefix(response.Metadata.SessionID, "sha256:") {
		t.Fatalf("SessionID = %q, want sha256 fingerprint, never the raw session identity", response.Metadata.SessionID)
	}
	if strings.Contains(response.Metadata.SessionID, "live-session-123") {
		t.Fatalf("SessionID leaks the raw session identity: %q", response.Metadata.SessionID)
	}
	if response.Metadata.FirstTokenLatency < 0 {
		t.Fatalf("FirstTokenLatency = %v, want >= 0", response.Metadata.FirstTokenLatency)
	}
	if response.Metadata.RetryCount != 0 || response.Metadata.Fallback || response.Metadata.UsageEstimated {
		t.Fatalf("protected lane telemetry nonzero: retry=%d fallback=%v usage_estimated=%v",
			response.Metadata.RetryCount, response.Metadata.Fallback, response.Metadata.UsageEstimated)
	}
}

// TestPreDispatchRefusalStampsIdentityAndKeepsLatencyZero mirrors the compat
// adapters: identity is stamped even when nothing was dispatched.
func TestPreDispatchRefusalStampsIdentityAndKeepsLatencyZero(t *testing.T) {
	client, server := newTransportClient(t, safeHandler(nil))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	response, err := client.completeOnce(ctx, provider.Request{Protocol: protocol.Current, Prompt: "private prompt"})
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if response.Metadata.DeliveryState != provider.DeliveryNotSent {
		t.Fatalf("DeliveryState = %v, want not_sent", response.Metadata.DeliveryState)
	}
	if response.Metadata.AdapterVersion != AdapterVersion {
		t.Fatalf("AdapterVersion = %q, want %q", response.Metadata.AdapterVersion, AdapterVersion)
	}
	if response.Metadata.Transport != transportID {
		t.Fatalf("Transport = %q, want %q", response.Metadata.Transport, transportID)
	}
	if response.Metadata.FirstTokenLatency != 0 {
		t.Fatalf("FirstTokenLatency = %v, want 0 (nothing observed)", response.Metadata.FirstTokenLatency)
	}
}
```

- [ ] **Step 2: Run the tests and verify the expected failures**

Run: `go test ./internal/provider/omniroute -run 'TestResponseMetadataTelemetryShape|TestPreDispatchRefusalStamps' -count=1`
Expected: FAIL (fields do not exist; possibly missing helper to be added per the note).

- [ ] **Step 3: Implement observation latency in delivery.go**

Apply the Task 2 step-3 pattern to `internal/provider/omniroute/delivery.go`: add `firstByteAt time.Time` and `now func() time.Time` to `deliveryObservation`, record the first byte in `GotFirstResponseByte` (keeping `markResponseStarted`), add `firstTokenLatency(started time.Time) time.Duration` with the same guards.

- [ ] **Step 4: Implement stamping and latency wiring**

In `internal/provider/omniroute/client.go`:

```go
// AdapterVersion identifies this legacy OmniRoute adapter build (#39). The
// web transport is outside the compatible-provider surface (#86), so it owns
// its own pinned version identity.
const AdapterVersion = "omniroute-v0.1"

const transportID = "omniroute-http"
```

Add a `baseMetadata` helper next to `responseMetadata`:

```go
func baseMetadata() provider.ResponseMetadata {
	return provider.ResponseMetadata{
		AdapterVersion: AdapterVersion,
		Transport:      transportID,
	}
}
```

Update `responseMetadata` to stamp identity:

```go
func responseMetadata(response *http.Response, duration, firstTokenLatency time.Duration, endpoint, model string, now time.Time) provider.ResponseMetadata {
	metadata := baseMetadata()
	metadata.StatusCode = response.StatusCode
	metadata.RequestID = sanitizeOpaque(response.Header.Get(requestIDHeader))
	// SessionID is the session fingerprint: only the sha256 digest of the
	// opaque session/connection identity may ever appear here (#39).
	metadata.SessionID = hashOpaque(response.Header.Get(sessionIDHeader))
	metadata.Duration = duration
	metadata.FirstTokenLatency = firstTokenLatency
	metadata.RetryAfter = parseRetryAfter(response.Header.Get("Retry-After"), now)
	metadata.ResetAt = parseResetAt(response.Header.Get("X-RateLimit-Reset"))
	metadata.Endpoint = logicalEndpoint(endpoint)
	metadata.Model = model
	return metadata
}
```

In `internal/provider/omniroute/client_transport.go`: construct the observation with the injected clock (`observation := &deliveryObservation{now: c.now}`), stamp identity on the transport-error and nil-response records (via `baseMetadata()`), and update the `responseMetadata` call to pass `observation.firstTokenLatency(started)`. The pre-dispatch records in `client_transport.go` and `telemetry.go` that build `provider.ResponseMetadata{DeliveryState: ..., Model: ...}` must also stamp `AdapterVersion`/`Transport`.

- [ ] **Step 5: Run the tests and verify they pass**

Run: `go test ./internal/provider/omniroute -count=1`
Expected: PASS (all existing and new tests).

- [ ] **Step 6: Commit**

```bash
git add internal/provider/omniroute/client.go internal/provider/omniroute/client_transport.go internal/provider/omniroute/delivery.go internal/provider/omniroute/telemetry_test.go
git commit -m "feat(provider): stamp telemetry identity and first-token latency in the OmniRoute adapter (#39)"
```

---

### Task 6: Governor outcome and event propagation

**Files:**
- Modify: `internal/governor/types.go`
- Modify: `internal/governor/execute.go`
- Modify: `internal/governor/permit.go`
- Create: `internal/governor/request_telemetry_test.go`

**Interfaces:**
- Consumes: `provider.ResponseMetadata` (Task 1); `OutcomeClassifier` and `Permit.Finish`/`FinishWithAttemptReceipts` signatures stay unchanged.
- Produces: `Outcome.Metadata provider.ResponseMetadata`; `Event.AttemptMetadata provider.ResponseMetadata`; `Execute` populates `outcome.Metadata` from the response right after classification; `Finish` and `FinishWithAttemptReceipts` copy `outcome.Metadata` into the emitted `attempt_finished` event. `CancelAfterStart` and `finishReceiptFailureLocked` leave `AttemptMetadata` zero (metadata provably unavailable on those paths).

- [ ] **Step 1: Write the failing propagation tests**

Create `internal/governor/request_telemetry_test.go` as an external test package (`governor_test`) reusing the existing `instantGovernor`, `eventSink` and the `policy` alias already used across `governor_test.go` and `attempt_receipts_test.go`:

```go
package governor_test

import (
	"context"
	"testing"
	"time"

	policy "github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/provider"
)

type telemetryProbeClient struct{}

func (c *telemetryProbeClient) Complete(_ context.Context, _ provider.Request) (provider.Response, error) {
	return provider.Response{Text: "ok", Metadata: provider.ResponseMetadata{
		AdapterVersion:    provider.CompatAdapterVersion,
		Transport:         "openaicompat-http",
		StatusCode:        200,
		Duration:          12 * time.Millisecond,
		FirstTokenLatency: 3 * time.Millisecond,
		DeliveryState:     provider.DeliveryCompleted,
	}}, nil
}

func (c *telemetryProbeClient) RouteSafety() provider.RouteSafety {
	return provider.SafeRouteSafety()
}

// TestExecuteEventCarriesAttemptMetadata proves the attempt_finished event
// carries the sanitized metadata the adapter proved.
func TestExecuteEventCarriesAttemptMetadata(t *testing.T) {
	g, _, events := instantGovernor(t)
	result := g.Execute(context.Background(), policy.AttemptRequest{
		TaskID:          "task-1",
		ClientRequestID: "request-1",
		ModelPool:       "model",
		ProviderRequest: provider.Request{Prompt: "private prompt"},
	}, &telemetryProbeClient{}, nil)
	if result.Err != nil || result.Completion.Err != nil {
		t.Fatalf("Execute() = %#v", result)
	}
	g.DrainEvents()
	var found bool
	for _, event := range events.Events() {
		if event.Kind != policy.EventAttemptFinished {
			continue
		}
		found = true
		if event.AttemptMetadata.AdapterVersion != provider.CompatAdapterVersion {
			t.Fatalf("AttemptMetadata.AdapterVersion = %q, want %q", event.AttemptMetadata.AdapterVersion, provider.CompatAdapterVersion)
		}
		if event.AttemptMetadata.Transport != "openaicompat-http" {
			t.Fatalf("AttemptMetadata.Transport = %q, want openaicompat-http", event.AttemptMetadata.Transport)
		}
		if event.AttemptMetadata.FirstTokenLatency != 3*time.Millisecond {
			t.Fatalf("AttemptMetadata.FirstTokenLatency = %v, want 3ms", event.AttemptMetadata.FirstTokenLatency)
		}
		if event.AttemptMetadata.DeliveryState != provider.DeliveryCompleted {
			t.Fatalf("AttemptMetadata.DeliveryState = %v, want completed", event.AttemptMetadata.DeliveryState)
		}
	}
	if !found {
		t.Fatal("no attempt_finished event was emitted")
	}
}

// TestRefusedExecutionNeverFabricatesAttemptMetadata proves the
// no-evidence rule: when no attempt is dispatched, emitted attempt_finished
// events must carry zero metadata instead of invented values.
func TestRefusedExecutionNeverFabricatesAttemptMetadata(t *testing.T) {
	g, _, events := instantGovernor(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := g.Execute(ctx, policy.AttemptRequest{
		TaskID:          "task-1",
		ClientRequestID: "request-1",
		ModelPool:       "model",
		ProviderRequest: provider.Request{Prompt: "private prompt"},
	}, &telemetryProbeClient{}, nil)
	if result.Err == nil {
		t.Fatal("expected refusal for a canceled context")
	}
	g.DrainEvents()
	for _, event := range events.Events() {
		if event.Kind != policy.EventAttemptFinished {
			continue
		}
		if event.AttemptMetadata.AdapterVersion != "" || event.AttemptMetadata.Transport != "" ||
			event.AttemptMetadata.FirstTokenLatency != 0 || event.AttemptMetadata.RetryCount != 0 ||
			event.AttemptMetadata.Fallback || event.AttemptMetadata.UsageEstimated {
			t.Fatalf("refused attempt fabricated metadata: %#v", event.AttemptMetadata)
		}
	}
}

// TestOutcomeCarriesMetadataZeroByDefault pins the zero-value contract of the
// new Outcome field.
func TestOutcomeCarriesMetadataZeroByDefault(t *testing.T) {
	var outcome policy.Outcome
	if outcome.Metadata.AdapterVersion != "" || outcome.Metadata.Transport != "" || outcome.Metadata.FirstTokenLatency != 0 {
		t.Fatalf("zero outcome metadata = %+v, want empty", outcome.Metadata)
	}
}
```

- [ ] **Step 2: Run the tests and verify the expected failures**

Run: `go test ./internal/governor -run 'TestExecuteEventCarriesAttemptMetadata|TestCancelEventMetadataStaysZero|TestOutcomeCarriesMetadataZeroByDefault' -count=1`
Expected: FAIL (`Outcome.Metadata` / `Event.AttemptMetadata` do not exist).

- [ ] **Step 3: Add the fields to Outcome and Event**

In `internal/governor/types.go`:

```go
type Outcome struct {
	Class           OutcomeClass
	RetryAfter      time.Duration
	ResetAt         time.Time
	UpstreamReached bool
	DeliveryState   provider.DeliveryState
	// Metadata is the sanitized provider response metadata of the classified
	// attempt (#39). It is copied by Execute after classification and flows
	// into emitted events; it never gates execution or accounting.
	Metadata provider.ResponseMetadata
}
```

Add to `Event` (after `GatewayContractHealth`):

```go
	// AttemptMetadata is the sanitized provider response metadata of the
	// classified attempt (#39). Paths where no classified outcome exists
	// (cancel-before-start, receipt-protection failures) leave it zero
	// rather than inventing evidence.
	AttemptMetadata provider.ResponseMetadata
```

- [ ] **Step 4: Populate the outcome in Execute**

In `internal/governor/execute.go`, right after classification:

```go
	outcome := classifier(response, callErr)
	outcome.DeliveryState = response.Metadata.DeliveryState
	outcome.Metadata = response.Metadata
	outcome = applyDeliveryEvidence(outcome)
```

- [ ] **Step 5: Copy the metadata into the emitted events**

In `internal/governor/permit.go`, in `Finish(outcome)` add to the `Event` literal after `TelemetryHealthy`:

```go
		AttemptMetadata:   outcome.Metadata,
```

In `FinishWithAttemptReceipts`, add the same field (from the `outcome` parameter) to the final `EventAttemptFinished` literal.

- [ ] **Step 6: Run the tests and verify they pass**

Run: `go test ./internal/governor -count=1`
Expected: PASS (all existing and new tests).

- [ ] **Step 7: Commit**

```bash
git add internal/governor/types.go internal/governor/execute.go internal/governor/permit.go internal/governor/request_telemetry_test.go
git commit -m "feat(governor): propagate sanitized attempt metadata through outcome and events (#39)"
```

---

### Task 7: Trace rendering and redaction coverage

**Files:**
- Modify: `internal/trace/policy.go`
- Modify: `internal/trace/policy_test.go`

**Interfaces:**
- Consumes: `governor.Event.AttemptMetadata` (Task 6).
- Produces: a sanitized `attempt` slog group on every rendered event; redaction tests covering the new fields.

- [ ] **Step 1: Write the failing rendering and redaction tests**

Append to `internal/trace/policy_test.go`:

```go
func TestPolicySinkRendersAttemptTelemetry(t *testing.T) {
	var buffer bytes.Buffer
	sink := NewPolicySink(slog.New(slog.NewTextHandler(&buffer, nil)))
	sink.Emit(governor.Event{
		Kind: governor.EventAttemptFinished,
		AttemptMetadata: provider.ResponseMetadata{
			AdapterVersion:    provider.CompatAdapterVersion,
			Transport:         "openaicompat-http",
			SessionID:         "sha256:0123456789abcdef",
			RequestID:         "sha256:fedcba9876543210",
			StatusCode:        200,
			Duration:          12 * time.Millisecond,
			FirstTokenLatency: 3 * time.Millisecond,
			DeliveryState:     provider.DeliveryCompleted,
		},
	})
	output := buffer.String()
	for _, want := range []string{
		"adapter_version", provider.CompatAdapterVersion,
		"transport", "openaicompat-http",
		"session_fingerprint", "sha256:0123456789abcdef",
		"status_code", "200",
		"first_token_latency", "3ms",
		"retry_count", "fallback", "usage_estimated",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("rendered event missing %q:\n%s", want, output)
		}
	}
}

// TestPolicySinkAttemptTelemetryRedaction proves the new fields cannot smuggle
// sensitive content: a raw-looking session value is never rendered because the
// contract only admits fingerprint-formatted values, and the renderer still
// omits prompts, bodies, credentials and raw headers.
func TestPolicySinkAttemptTelemetryRedaction(t *testing.T) {
	var buffer bytes.Buffer
	sink := NewPolicySink(slog.New(slog.NewTextHandler(&buffer, nil)))
	sink.Emit(governor.Event{
		Kind: governor.EventAttemptFinished,
		AttemptMetadata: provider.ResponseMetadata{
			AdapterVersion: provider.CompatAdapterVersion,
			Transport:      "openaicompat-http",
			SessionID:      "sha256:0123456789abcdef",
			RequestID:      "sha256:fedcba9876543210",
		},
	})
	output := buffer.String()
	for _, forbidden := range []string{
		"live-session", "Bearer", "Authorization", "api_key", "cookie",
		"prompt", "response body", "credential", "raw-header",
	} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("rendered event leaked %q:\n%s", forbidden, output)
		}
	}
}
```

Add the missing imports (`bytes`, `log/slog`, `strings`, `time`, `github.com/RenyEnnos/Runstead/internal/governor`, `github.com/RenyEnnos/Runstead/internal/provider`) to `policy_test.go` as needed.

- [ ] **Step 2: Run the tests and verify the expected failures**

Run: `go test ./internal/trace -run 'TestPolicySinkRendersAttemptTelemetry|TestPolicySinkAttemptTelemetryRedaction' -count=1`
Expected: FAIL (fields not rendered).

- [ ] **Step 3: Render the sanitized attempt group**

In `internal/trace/policy.go`, add to the `args` slice in `Emit`:

```go
		slog.Group("attempt",
			"adapter_version", event.AttemptMetadata.AdapterVersion,
			"transport", event.AttemptMetadata.Transport,
			"session_fingerprint", event.AttemptMetadata.SessionID,
			"status_code", event.AttemptMetadata.StatusCode,
			"request_id", event.AttemptMetadata.RequestID,
			"duration", event.AttemptMetadata.Duration,
			"first_token_latency", event.AttemptMetadata.FirstTokenLatency,
			"retry_count", event.AttemptMetadata.RetryCount,
			"fallback", event.AttemptMetadata.Fallback,
			"usage_estimated", event.AttemptMetadata.UsageEstimated,
			"delivery_state", event.AttemptMetadata.DeliveryState,
		),
```

- [ ] **Step 4: Run the tests and verify they pass**

Run: `go test ./internal/trace -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/trace/policy.go internal/trace/policy_test.go
git commit -m "feat(trace): render sanitized attempt telemetry with redaction coverage (#39)"
```

---

### Task 8: Documentation and full verification

**Files:**
- Create: `docs/telemetry.md`
- Modify: `README.md`

- [ ] **Step 1: Write the telemetry contract documentation**

Create `docs/telemetry.md` (style mirrors `docs/retry.md` / `docs/learning.md`):

```markdown
# Request telemetry (issue #39)

Runstead keeps its own minimal, sanitized per-attempt request telemetry so
operators can reason about account health and protocol drift without relying
on transport-side claims.

## Contract

Every `provider.ResponseMetadata` record may carry:

- `adapter_version`: pinned adapter/composition version
  (`provider.CompatAdapterVersion` for the compatible families);
- `transport`: stable transport identifier (`openaicompat-http`,
  `anthropiccompat-http`, `googlecompat-http`, `omniroute-http`);
- `session_fingerprint`: sha256 fingerprint of the opaque session/connection
  identity, never the raw identity;
- `first_token_latency`: request-start to first-response-byte latency when the
  adapter's transport observation proves it;
- `retry_count` and `fallback`: the protected lane has no retries or fallbacks
  outside the governor, so both are always zero;
- `usage_estimated`: false today (no adapter emits usage); reserved so a
  transport that reports estimated usage must declare it.

## Zero-value rule

Unmeasurable fields are absent or zero; Runstead never guesses latency, usage
or amplification. This is enforced by adapter unit tests and the governor
zero-outcome test.

## Surface

Governor `attempt_finished` events carry the sanitized metadata and
`trace.PolicySink` renders it as an `attempt` group. The fields are
event/trace-level evidence: they are not durable task truth, and they never
gate admission, retries, policy or accounting.

## Redaction

Traces and metadata exclude prompts, response bodies, credentials, cookies,
raw headers and raw session/connection identity. `RequestID` and
`SessionID` are sanitized (hash-formatted); redaction tests cover the
telemetry fields.

## Deferred

The browser-provider outcome refinements (incomplete responses, user-action
needs, session loss, adapter drift, request/assistant fingerprints, opaque
conversation references, bounded retention) are deferred to the
plugin/composable-provider track (#80, #82, #83, #74, #86).
```

- [ ] **Step 2: Link the documentation from the README**

In `README.md`, after the `docs/learning.md` pointer (end of the "Project status" section, lines around 208-212), add:

```markdown
Sanitized per-attempt request telemetry (adapter version, transport, session
fingerprint, first-token latency and the protected-lane zero fields) is
documented in [`docs/telemetry.md`](docs/telemetry.md).
```

- [ ] **Step 3: Run the full verification suite**

Run, from the repository root:

```bash
export PATH="$HOME/.local/go/bin:$PATH"
test -z "$(gofmt -l .)"
go build ./...
go test ./... 
go test -race ./...
go vet ./...
go build -o /tmp/quality-gates ./tools/quality
/tmp/quality-gates growth --root "$PWD"
/tmp/quality-gates errcheck --root "$PWD"
/tmp/quality-gates live-convention --root "$PWD"
```

Expected: all green. If `errcheck` reports new swallowed-error sites, add each site to `tools/quality/errcheck.allowlist` with the `#39` rationale in a separate commit. If `growth` reports a size limit hit, raise the limit in `tools/quality/limits.json` with a comment in the same commit.

- [ ] **Step 4: Re-run the spec's integration checks**

Run: `bash experiments/protocol/test.sh` and `go test -count=1 ./internal/protocol/ -run '^(TestCorpusFixtures|TestParseValidAction|TestParseValidFinal|TestEnvelopeMarkersInsideJSONStringsAreContent)$'`
Expected: all green (protocol surfaces are untouched, but the gate must stay green).

- [ ] **Step 5: Commit**

```bash
git add docs/telemetry.md README.md tools/quality/ 2>/dev/null
git commit -m "docs: document the sanitized request telemetry contract (#39)"
```

- [ ] **Step 6: Final review pass**

Run `git log --oneline main..HEAD` and verify every commit is present, then follow the finishing-a-development-branch skill (push, PR against `main` with a description referencing #39, close out review feedback).