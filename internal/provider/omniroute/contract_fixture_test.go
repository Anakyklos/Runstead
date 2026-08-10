package omniroute

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

// The embedded corpus is deliberately test-only. It cannot be reached by
// production code or by a live OmniRoute client.
//
//go:embed testdata/contract
var embeddedContractCorpus embed.FS

type contractManifest struct {
	SchemaVersion      int                          `json:"schema_version"`
	ManagementDefaults map[string]string            `json:"management_defaults"`
	ErrorKindInventory []contractErrorKindInventory `json:"error_kind_inventory"`
	Scenarios          []contractScenario           `json:"scenarios"`
}

type contractErrorKindInventory struct {
	Kind       string   `json:"kind"`
	Categories []string `json:"categories"`
	Scenarios  []string `json:"scenarios"`
}

type contractScenario struct {
	Name       string                `json:"name"`
	Operation  string                `json:"operation"`
	Transport  contractTransportSpec `json:"transport,omitempty"`
	Response   *contractResponseSpec `json:"response,omitempty"`
	Management map[string]string     `json:"management,omitempty"`
	Config     contractConfigSpec    `json:"config,omitempty"`
	Request    contractRequestSpec   `json:"request,omitempty"`
	Expected   contractExpectation   `json:"expected"`
}

type contractResponseSpec struct {
	Status      int               `json:"status"`
	BodyFile    string            `json:"body_file,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	ReceiptFile string            `json:"receipt_file,omitempty"`
}

type contractTransportSpec struct {
	Kind     string `json:"kind,omitempty"`
	DelayMS  int    `json:"delay_ms,omitempty"`
	Bytes    int    `json:"bytes,omitempty"`
	Location string `json:"location,omitempty"`
}

type contractConfigSpec struct {
	MaxRequestBytes  int  `json:"max_request_bytes,omitempty"`
	MaxResponseBytes int  `json:"max_response_bytes,omitempty"`
	TimeoutMS        int  `json:"timeout_ms,omitempty"`
	AttemptReceipts  bool `json:"attempt_receipts,omitempty"`
}

type contractRequestSpec struct {
	Prompt          string `json:"prompt,omitempty"`
	Model           string `json:"model,omitempty"`
	ClientRequestID string `json:"client_request_id,omitempty"`
}

type contractExpectation struct {
	ErrorKind            string         `json:"error_kind"`
	OutcomeClass         string         `json:"outcome_class,omitempty"`
	DeliveryState        string         `json:"delivery_state,omitempty"`
	UpstreamReached      *bool          `json:"upstream_reached,omitempty"`
	RetryAfterSeconds    int            `json:"retry_after_seconds,omitempty"`
	ResetAt              string         `json:"reset_at,omitempty"`
	ErrorRetryAfter      int            `json:"error_retry_after_seconds,omitempty"`
	ErrorResetAt         string         `json:"error_reset_at,omitempty"`
	ClassifiedRetryAfter int            `json:"classified_retry_after_seconds,omitempty"`
	ClassifiedResetAt    string         `json:"classified_reset_at,omitempty"`
	ResponseText         string         `json:"response_text,omitempty"`
	RequestID            string         `json:"request_id,omitempty"`
	SessionIDHash        string         `json:"session_id_hash,omitempty"`
	RequestCounts        map[string]int `json:"request_counts,omitempty"`
	AttemptReceiptCount  int            `json:"attempt_receipt_count,omitempty"`
}

const contractSchemaVersion = 1

var contractManagementEndpoints = map[string]string{
	"resilience":             "/api/resilience",
	"rate_limits":            "/api/rate-limits",
	"settings":               "/api/settings",
	"model_aliases":          "/api/models/alias",
	"settings_model_aliases": "/api/settings/model-aliases",
	"fallback_chains":        "/api/fallback/chains",
	"combos":                 "/api/combos",
	"model_combo_mappings":   "/api/model-combo-mappings",
	"providers":              "/api/providers",
}

var contractCategories = map[string]struct{}{
	"http_body_header":        {},
	"transport":               {},
	"pre_request":             {},
	"route_safety_management": {},
	"attempt_receipts":        {},
}

var contractOperations = map[string]struct{}{
	"complete_once": {},
	"complete":      {},
	"preflight":     {},
	"snapshot":      {},
}

var contractTransports = map[string]struct{}{
	"":                   {},
	"cancelled":          {},
	"dial_error":         {},
	"connection_reset":   {},
	"delay":              {},
	"redirect":           {},
	"oversized_response": {},
	"close_connection":   {},
}

var (
	contractCredentialFieldPattern = regexp.MustCompile(`(?i)(?:^|["'\s])(?:authorization|proxy-authorization|cookie|set-cookie|x-api-key|api[-_]key|access[-_]?token|refresh[-_]?token|session[-_]?token|id[-_]?token)(?:["'\s]|:)`)
	contractBearerPattern          = regexp.MustCompile(`(?i)\bbearer\s+[a-z0-9._~+/=-]{12,}`)
	contractAPIKeyPattern          = regexp.MustCompile(`(?i)\b(?:sk|ghp|github_pat|xox[baprs])[-_][a-z0-9_-]{12,}\b`)
	contractJWTPattern             = regexp.MustCompile(`\beyJ[a-zA-Z0-9_-]{10,}\.[a-zA-Z0-9_-]{10,}\.[a-zA-Z0-9_-]{10,}\b`)
	contractEmailPattern           = regexp.MustCompile(`(?i)\b[a-z0-9][a-z0-9._%+\-]{0,63}@[a-z0-9.-]+\.[a-z]{2,}\b`)
)

func writeFixtureFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeManifest(t *testing.T, root, content string) {
	t.Helper()
	writeFixtureFile(t, root, "manifest.json", content)
}

func contractFixtureFS() fs.FS {
	root, err := fs.Sub(embeddedContractCorpus, "testdata/contract")
	if err != nil {
		panic(err)
	}
	return root
}

func contractFixtureBytes(name string) ([]byte, error) {
	return fs.ReadFile(contractFixtureFS(), filepath.ToSlash(name))
}

func mustReadContractFixture(name string) string {
	data, err := contractFixtureBytes(name)
	if err != nil {
		panic(fmt.Sprintf("read contract fixture %q: %v", name, err))
	}
	return string(data)
}

func scanFixtureHygiene(fsys fs.FS) error {
	return fs.WalkDir(fsys, ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return fmt.Errorf("read fixture %q: %w", path, err)
		}
		text := string(data)
		if contractCredentialFieldPattern.MatchString(text) {
			return fmt.Errorf("fixture %q contains a credential field or header", path)
		}
		for name, pattern := range map[string]*regexp.Regexp{
			"bearer credential": contractBearerPattern,
			"API key":           contractAPIKeyPattern,
			"JWT credential":    contractJWTPattern,
			"email identifier":  contractEmailPattern,
		} {
			if pattern.MatchString(text) {
				return fmt.Errorf("fixture %q contains a secret-shaped %s", path, name)
			}
		}
		return nil
	})
}

func loadContractManifest(fsys fs.FS) (contractManifest, error) {
	if err := scanFixtureHygiene(fsys); err != nil {
		return contractManifest{}, err
	}
	data, err := fs.ReadFile(fsys, "manifest.json")
	if err != nil {
		return contractManifest{}, fmt.Errorf("read contract manifest: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var manifest contractManifest
	if err := decoder.Decode(&manifest); err != nil {
		return contractManifest{}, fmt.Errorf("decode contract manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return contractManifest{}, fmt.Errorf("contract manifest has trailing data")
	}
	if manifest.SchemaVersion != contractSchemaVersion {
		return contractManifest{}, fmt.Errorf("unsupported contract manifest schema version %d", manifest.SchemaVersion)
	}
	sourceKinds, err := errorKindsFromSource()
	if err != nil {
		return contractManifest{}, err
	}

	seenScenarios := make(map[string]struct{}, len(manifest.Scenarios))
	for index, scenario := range manifest.Scenarios {
		if strings.TrimSpace(scenario.Name) == "" {
			return contractManifest{}, fmt.Errorf("scenario %d has no name", index)
		}
		if _, exists := seenScenarios[scenario.Name]; exists {
			return contractManifest{}, fmt.Errorf("duplicate contract scenario %q", scenario.Name)
		}
		seenScenarios[scenario.Name] = struct{}{}
		if _, ok := contractOperations[scenario.Operation]; !ok {
			return contractManifest{}, fmt.Errorf("scenario %q has unknown operation %q", scenario.Name, scenario.Operation)
		}
		if _, ok := contractTransports[scenario.Transport.Kind]; !ok {
			return contractManifest{}, fmt.Errorf("scenario %q has unknown transport %q", scenario.Name, scenario.Transport.Kind)
		}
		if expectedKind := ErrorKind(strings.TrimSpace(scenario.Expected.ErrorKind)); expectedKind != "" {
			if _, ok := sourceKinds[expectedKind]; !ok {
				return contractManifest{}, fmt.Errorf("scenario %q has unknown expected ErrorKind %q", scenario.Name, expectedKind)
			}
		}
		if scenario.Response != nil {
			if err := validateFixtureReference(fsys, scenario.Response.BodyFile); err != nil {
				return contractManifest{}, fmt.Errorf("scenario %q response: %w", scenario.Name, err)
			}
			if err := validateFixtureReference(fsys, scenario.Response.ReceiptFile); err != nil {
				return contractManifest{}, fmt.Errorf("scenario %q receipt: %w", scenario.Name, err)
			}
		}
		for endpoint, fixture := range scenario.Management {
			if _, ok := contractManagementEndpoints[endpoint]; !ok {
				return contractManifest{}, fmt.Errorf("scenario %q has unknown management endpoint %q", scenario.Name, endpoint)
			}
			if err := validateFixtureReference(fsys, fixture); err != nil {
				return contractManifest{}, fmt.Errorf("scenario %q management %q: %w", scenario.Name, endpoint, err)
			}
		}
	}
	for endpoint, fixture := range manifest.ManagementDefaults {
		if _, ok := contractManagementEndpoints[endpoint]; !ok {
			return contractManifest{}, fmt.Errorf("unknown management endpoint %q", endpoint)
		}
		if err := validateFixtureReference(fsys, fixture); err != nil {
			return contractManifest{}, fmt.Errorf("management endpoint %q: %w", endpoint, err)
		}
	}
	for endpoint := range contractManagementEndpoints {
		if _, ok := manifest.ManagementDefaults[endpoint]; !ok {
			return contractManifest{}, fmt.Errorf("management endpoint %q is missing from manifest defaults", endpoint)
		}
	}

	manifestKinds := make(map[ErrorKind]struct{}, len(manifest.ErrorKindInventory))
	scenariosByName := make(map[string]contractScenario, len(manifest.Scenarios))
	for _, scenario := range manifest.Scenarios {
		scenariosByName[scenario.Name] = scenario
	}
	for index, entry := range manifest.ErrorKindInventory {
		kind := ErrorKind(strings.TrimSpace(entry.Kind))
		if kind == "" {
			return contractManifest{}, fmt.Errorf("inventory entry %d has no error kind", index)
		}
		if _, exists := manifestKinds[kind]; exists {
			return contractManifest{}, fmt.Errorf("duplicate inventory error kind %q", kind)
		}
		manifestKinds[kind] = struct{}{}
		for _, category := range entry.Categories {
			if _, ok := contractCategories[category]; !ok {
				return contractManifest{}, fmt.Errorf("inventory error kind %q has unknown category %q", kind, category)
			}
		}
		for _, scenarioName := range entry.Scenarios {
			scenario, ok := scenariosByName[scenarioName]
			if !ok {
				return contractManifest{}, fmt.Errorf("inventory error kind %q references unknown scenario %q", kind, scenarioName)
			}
			if scenario.Expected.ErrorKind != string(kind) {
				return contractManifest{}, fmt.Errorf("inventory error kind %q references scenario %q with expected kind %q", kind, scenarioName, scenario.Expected.ErrorKind)
			}
		}
	}
	for kind := range sourceKinds {
		if _, ok := manifestKinds[kind]; !ok {
			return contractManifest{}, fmt.Errorf("ErrorKind %q is missing from contract inventory", kind)
		}
	}
	for kind := range manifestKinds {
		if _, ok := sourceKinds[kind]; !ok {
			return contractManifest{}, fmt.Errorf("contract inventory contains unknown ErrorKind %q", kind)
		}
	}
	return manifest, nil
}

func validateFixtureReference(fsys fs.FS, name string) error {
	if strings.TrimSpace(name) == "" {
		return nil
	}
	clean := filepath.ToSlash(filepath.Clean(name))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
		return fmt.Errorf("invalid fixture path %q", name)
	}
	if _, err := fs.Stat(fsys, clean); err != nil {
		return fmt.Errorf("unknown fixture %q: %w", name, err)
	}
	return nil
}

func errorKindsFromSource() (map[ErrorKind]struct{}, error) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		return nil, fmt.Errorf("locate contract fixture test source")
	}
	path := filepath.Join(filepath.Dir(sourceFile), "errors.go")
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		return nil, fmt.Errorf("parse ErrorKind source: %w", err)
	}
	kinds := make(map[ErrorKind]struct{})
	for _, declaration := range file.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok || general.Tok != token.CONST {
			continue
		}
		for _, specification := range general.Specs {
			values, ok := specification.(*ast.ValueSpec)
			if !ok || values.Type == nil {
				continue
			}
			identifier, ok := values.Type.(*ast.Ident)
			if !ok || identifier.Name != "ErrorKind" {
				continue
			}
			for index, name := range values.Names {
				if index < len(values.Values) {
					literal, ok := values.Values[index].(*ast.BasicLit)
					if ok && literal.Kind == token.STRING {
						value, err := strconv.Unquote(literal.Value)
						if err == nil {
							kinds[ErrorKind(value)] = struct{}{}
							continue
						}
					}
				}
				kinds[ErrorKind(name.Name)] = struct{}{}
			}
		}
	}
	if len(kinds) == 0 {
		return nil, fmt.Errorf("no ErrorKind constants found in %s", path)
	}
	return kinds, nil
}

func TestContractCorpusScenarios(t *testing.T) {
	manifest, err := loadContractManifest(contractFixtureFS())
	if err != nil {
		t.Fatalf("loadContractManifest() error = %v", err)
	}
	for _, scenario := range manifest.Scenarios {
		scenario := scenario
		t.Run(scenario.Name, func(t *testing.T) {
			runContractScenario(t, manifest, scenario)
		})
	}
}

func runContractScenario(t *testing.T, manifest contractManifest, scenario contractScenario) {
	t.Helper()
	started := time.Now()
	now := time.Date(2026, time.August, 10, 20, 0, 0, 0, time.UTC)
	responseSpec := contractMockResponse{transport: scenario.Transport}
	if scenario.Response != nil {
		responseSpec.status = scenario.Response.Status
		responseSpec.headers = make(http.Header, len(scenario.Response.Headers))
		for key, value := range scenario.Response.Headers {
			responseSpec.headers.Set(key, value)
		}
		if scenario.Response.BodyFile != "" {
			responseSpec.body = []byte(mustReadContractFixture(scenario.Response.BodyFile))
		}
		if scenario.Response.ReceiptFile != "" {
			responseSpec.receipt = []byte(mustReadContractFixture(scenario.Response.ReceiptFile))
		}
	}
	managementDefaults := make(map[string]contractMockResponse, len(manifest.ManagementDefaults))
	for endpoint, fixture := range manifest.ManagementDefaults {
		managementDefaults[endpoint] = contractMockResponse{status: http.StatusOK, body: []byte(mustReadContractFixture(fixture))}
	}
	management := make(map[string]contractMockResponse, len(scenario.Management))
	for endpoint, fixture := range scenario.Management {
		management[endpoint] = contractMockResponse{status: http.StatusOK, body: []byte(mustReadContractFixture(fixture))}
	}

	var server *contractMockServer
	var roundTrips atomic.Int32
	var options Options
	var baseURL string
	if scenario.Transport.Kind == "dial_error" || scenario.Transport.Kind == "connection_reset" {
		baseURL = "http://contract.invalid"
		options.Transport = contractDialTransport(scenario.Transport.Kind, &roundTrips)
	} else {
		server = newContractMockServer(t, contractMockConfig{
			completion:         responseSpec,
			management:         management,
			managementDefaults: managementDefaults,
		})
		defer server.Close()
		baseURL = server.URL()
		options.HTTPClient = server.Client()
	}
	options.Now = func() time.Time { return now }

	config := testConfig(baseURL)
	if scenario.Config.MaxRequestBytes != 0 {
		config.MaxRequestBytes = scenario.Config.MaxRequestBytes
	}
	if scenario.Config.MaxResponseBytes != 0 {
		config.MaxResponseBytes = scenario.Config.MaxResponseBytes
	}
	if scenario.Config.TimeoutMS != 0 {
		config.Timeout = time.Duration(scenario.Config.TimeoutMS) * time.Millisecond
	}
	if scenario.Config.AttemptReceipts {
		config.EnableAttemptReceipts = true
		config.Provider = "chatgpt-web"
		config.AccountLaneHash = "lane-hash-synthetic-001"
		config.RouteSafety = provider.ReceiptRouteSafety()
	}
	client, err := New(config, options)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	request := provider.Request{
		Prompt:          scenario.Request.Prompt,
		Model:           scenario.Request.Model,
		ClientRequestID: scenario.Request.ClientRequestID,
	}
	if request.Prompt == "" {
		request.Prompt = "synthetic boundary prompt"
	}
	if scenario.Config.AttemptReceipts && request.ClientRequestID == "" {
		request.ClientRequestID = "contract-request-001"
	}
	ctx := context.Background()
	if scenario.Transport.Kind == "cancelled" {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		cancel()
	}

	var response provider.Response
	switch scenario.Operation {
	case "complete_once":
		response, err = client.completeOnce(ctx, request)
	case "complete":
		response, err = client.Complete(ctx, request)
	case "preflight":
		err = client.Preflight(ctx)
	case "snapshot":
		_, err = client.Snapshot(ctx)
	default:
		t.Fatalf("unsupported operation %q", scenario.Operation)
	}

	assertContractError(t, scenario.Expected.ErrorKind, err)
	if scenario.Operation == "preflight" && scenario.Expected.ErrorKind == string(ErrorUnsafeRoute) {
		if client.RouteSafety().Validate() == nil {
			t.Fatalf("RouteSafety after unsafe preflight = %#v, want fail-closed unknown state", client.RouteSafety())
		}
		_, completeErr := client.Complete(ctx, provider.Request{Prompt: "synthetic protected completion"})
		if !errors.Is(completeErr, provider.ErrUnsafeRoute) {
			t.Fatalf("Complete() after unsafe preflight = %v, want fail-closed unsafe route", completeErr)
		}
	}
	if scenario.Operation == "complete_once" || scenario.Operation == "complete" {
		outcome := Classify(response, err)
		if scenario.Expected.OutcomeClass != "" && string(outcome.Class) != scenario.Expected.OutcomeClass {
			t.Fatalf("Classify() class = %q, want %q (response=%#v err=%v)", outcome.Class, scenario.Expected.OutcomeClass, response, err)
		}
		if scenario.Expected.UpstreamReached != nil && outcome.UpstreamReached != *scenario.Expected.UpstreamReached {
			t.Fatalf("Classify() upstream reached = %t, want %t", outcome.UpstreamReached, *scenario.Expected.UpstreamReached)
		}
		if scenario.Expected.ErrorRetryAfter != 0 || scenario.Expected.ErrorResetAt != "" {
			var providerErr *Error
			if !errors.As(err, &providerErr) {
				t.Fatalf("expected typed Error for retry/reset assertions, got %T %v", err, err)
			}
			if scenario.Expected.ErrorRetryAfter != 0 && providerErr.RetryAfter != time.Duration(scenario.Expected.ErrorRetryAfter)*time.Second {
				t.Fatalf("typed Error RetryAfter = %s, want %ds", providerErr.RetryAfter, scenario.Expected.ErrorRetryAfter)
			}
			if scenario.Expected.ErrorResetAt != "" {
				resetAt, parseErr := time.Parse(time.RFC3339, scenario.Expected.ErrorResetAt)
				if parseErr != nil {
					t.Fatalf("manifest error_reset_at is invalid: %v", parseErr)
				}
				if !providerErr.ResetAt.Equal(resetAt) {
					t.Fatalf("typed Error ResetAt = %s, want %s", providerErr.ResetAt, resetAt)
				}
			}
		}
		if scenario.Expected.ClassifiedRetryAfter != 0 && outcome.RetryAfter != time.Duration(scenario.Expected.ClassifiedRetryAfter)*time.Second {
			t.Fatalf("Classify() RetryAfter = %s, want %ds", outcome.RetryAfter, scenario.Expected.ClassifiedRetryAfter)
		}
		if scenario.Expected.ClassifiedResetAt != "" {
			resetAt, parseErr := time.Parse(time.RFC3339, scenario.Expected.ClassifiedResetAt)
			if parseErr != nil {
				t.Fatalf("manifest classified_reset_at is invalid: %v", parseErr)
			}
			if !outcome.ResetAt.Equal(resetAt) {
				t.Fatalf("Classify() ResetAt = %s, want %s", outcome.ResetAt, resetAt)
			}
		}
		if scenario.Expected.DeliveryState != "" && response.Metadata.DeliveryState.String() != scenario.Expected.DeliveryState {
			t.Fatalf("delivery state = %q, want %q", response.Metadata.DeliveryState, scenario.Expected.DeliveryState)
		}
		if scenario.Expected.ResponseText != "" && response.Text != scenario.Expected.ResponseText {
			t.Fatalf("response text = %q, want %q", response.Text, scenario.Expected.ResponseText)
		}
		if scenario.Expected.RequestID != "" && response.Metadata.RequestID != scenario.Expected.RequestID {
			t.Fatalf("RequestID = %q, want %q", response.Metadata.RequestID, scenario.Expected.RequestID)
		}
		if scenario.Expected.SessionIDHash != "" && response.Metadata.SessionID != scenario.Expected.SessionIDHash {
			t.Fatalf("SessionID = %q, want exact sanitized hash %q", response.Metadata.SessionID, scenario.Expected.SessionIDHash)
		}
		if scenario.Expected.RetryAfterSeconds != 0 && response.Metadata.RetryAfter != time.Duration(scenario.Expected.RetryAfterSeconds)*time.Second {
			t.Fatalf("RetryAfter = %s, want %ds", response.Metadata.RetryAfter, scenario.Expected.RetryAfterSeconds)
		}
		if scenario.Expected.ResetAt != "" {
			resetAt, parseErr := time.Parse(time.RFC3339, scenario.Expected.ResetAt)
			if parseErr != nil {
				t.Fatalf("manifest reset_at is invalid: %v", parseErr)
			}
			if !response.Metadata.ResetAt.Equal(resetAt) {
				t.Fatalf("ResetAt = %s, want %s", response.Metadata.ResetAt, resetAt)
			}
		}
		if scenario.Expected.AttemptReceiptCount != 0 {
			if response.Metadata.AttemptReceipts == nil || len(response.Metadata.AttemptReceipts.Receipts) != scenario.Expected.AttemptReceiptCount {
				t.Fatalf("attempt receipts = %#v, want %d", response.Metadata.AttemptReceipts, scenario.Expected.AttemptReceiptCount)
			}
		}
		metadataText := fmt.Sprintf("%#v", response.Metadata)
		if strings.Contains(metadataText, "Bearer ") || strings.Contains(metadataText, config.APIKey) {
			t.Fatalf("response metadata leaked credential material: %s", metadataText)
		}
		if response.Metadata.SessionID == "synthetic-session-001" {
			t.Fatal("raw synthetic session identifier was not hashed")
		}
	}
	if err != nil {
		errText := err.Error()
		for _, private := range []string{config.APIKey, request.Prompt} {
			if private != "" && strings.Contains(errText, private) {
				t.Fatalf("error leaked private test input %q: %v", private, err)
			}
		}
		if responseSpec.body != nil && len(responseSpec.body) > 0 && strings.Contains(errText, string(responseSpec.body)) {
			t.Fatalf("error leaked synthetic response fixture body: %v", err)
		}
	}

	counts := map[string]int{"round_trips": int(roundTrips.Load())}
	if server != nil {
		for key, value := range server.Counts() {
			counts[key] = value
		}
	}
	for key, want := range scenario.Expected.RequestCounts {
		if got := counts[key]; got != want {
			t.Fatalf("request count %q = %d, want %d (all=%#v)", key, got, want, counts)
		}
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("scenario exceeded bounded test duration: %s", elapsed)
	}
}

func assertContractError(t *testing.T, wantKind string, err error) {
	t.Helper()
	if wantKind == "" {
		if err != nil {
			t.Fatalf("unexpected contract error: %v", err)
		}
		return
	}
	if err == nil {
		t.Fatalf("expected contract error kind %q, got nil", wantKind)
	}
	var providerErr *Error
	if !errors.As(err, &providerErr) {
		t.Fatalf("error %T %v is not an OmniRoute Error", err, err)
	}
	if string(providerErr.Kind) != wantKind {
		t.Fatalf("error kind = %q, want %q", providerErr.Kind, wantKind)
	}
}

func TestContractFixtureHygieneRejectsSecretShapedValues(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFile(t, dir, "responses/unsafe.json", `{"Authorization":"Bearer synthetic-secret-value-123456"}`)
	if err := scanFixtureHygiene(os.DirFS(dir)); err == nil {
		t.Fatal("scanFixtureHygiene() accepted a secret-shaped fixture")
	}
}

func TestContractFixtureHygieneAllowsSemanticTokenSignals(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFile(t, dir, "responses/safe.json", `{"error":{"code":"token_expired"},"classifier_code":"invalid_api_key","configuration_profile":"apikey","request_id":"opaque-request-001","session":"synthetic-session-001"}`)
	if err := scanFixtureHygiene(os.DirFS(dir)); err != nil {
		t.Fatalf("scanFixtureHygiene() rejected synthetic semantic signals: %v", err)
	}
}

func TestContractManifestRejectsMalformedManifest(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "{")
	if _, err := loadContractManifest(os.DirFS(dir)); err == nil {
		t.Fatal("loadContractManifest() accepted malformed JSON")
	}
}

func TestContractManifestRejectsUnknownFixture(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, `{
		"schema_version": 1,
		"management_defaults": {},
		"error_kind_inventory": [],
		"scenarios": [{
			"name": "unknown_fixture",
			"operation": "complete_once",
			"response": {"status": 200, "body_file": "responses/missing.json"},
			"expected": {"error_kind": ""}
		}]
	}`)
	if _, err := loadContractManifest(os.DirFS(dir)); err == nil {
		t.Fatal("loadContractManifest() accepted an unknown fixture reference")
	}
}

func TestContractManifestRejectsUnknownScenarioReference(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, `{
		"schema_version": 1,
		"management_defaults": {},
		"error_kind_inventory": [{"kind":"transport","categories":["transport"],"scenarios":["missing_scenario"]}],
		"scenarios": []
	}`)
	if _, err := loadContractManifest(os.DirFS(dir)); err == nil {
		t.Fatal("loadContractManifest() accepted an unknown scenario reference")
	}
}

func TestContractManifestRejectsMissingManagementDefault(t *testing.T) {
	dir := t.TempDir()
	copyEmbeddedContractFixtures(t, dir)
	manifest := mustReadContractFixture("manifest.json")
	manifest = strings.Replace(manifest, "    \"providers\": \"management/providers-safe.json\"\n", "", 1)
	writeManifest(t, dir, manifest)
	if _, err := loadContractManifest(os.DirFS(dir)); err == nil {
		t.Fatal("loadContractManifest() accepted a manifest with missing management defaults")
	}
}

func TestContractManifestRejectsUnknownExpectedKind(t *testing.T) {
	dir := t.TempDir()
	copyEmbeddedContractFixtures(t, dir)
	manifest := mustReadContractFixture("manifest.json")
	manifest = strings.Replace(manifest, `"expected": {"error_kind":"",`, `"expected": {"error_kind":"unknown_kind",`, 1)
	writeManifest(t, dir, manifest)
	if _, err := loadContractManifest(os.DirFS(dir)); err == nil {
		t.Fatal("loadContractManifest() accepted an unknown expected error kind")
	}
}

func copyEmbeddedContractFixtures(t *testing.T, root string) {
	t.Helper()
	err := fs.WalkDir(contractFixtureFS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(contractFixtureFS(), path)
		if err != nil {
			return err
		}
		writeFixtureFile(t, root, path, string(data))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestContractManifestInventoryMatchesErrorKinds(t *testing.T) {
	manifest, err := loadContractManifest(contractFixtureFS())
	if err != nil {
		t.Fatalf("loadContractManifest() error = %v", err)
	}
	sourceKinds, err := errorKindsFromSource()
	if err != nil {
		t.Fatalf("errorKindsFromSource() error = %v", err)
	}
	manifestKinds := make(map[ErrorKind]struct{}, len(manifest.ErrorKindInventory))
	for _, entry := range manifest.ErrorKindInventory {
		manifestKinds[ErrorKind(entry.Kind)] = struct{}{}
	}
	for kind := range sourceKinds {
		if _, ok := manifestKinds[kind]; !ok {
			t.Errorf("ErrorKind %q is missing from the contract inventory", kind)
		}
	}
	for kind := range manifestKinds {
		if _, ok := sourceKinds[kind]; !ok {
			t.Errorf("contract inventory contains unknown ErrorKind %q", kind)
		}
	}
}
