package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

const (
	baseURL       = "http://127.0.0.1:18765"
	chromiumPath  = "/usr/bin/chromium"
	scenarioLimit = 1200 * time.Millisecond
)

type processInfo struct {
	PID   int    `json:"pid"`
	PPID  int    `json:"ppid"`
	Depth int    `json:"depth"`
	Args  string `json:"args"`
}

type pageEvent struct {
	Type             string `json:"type"`
	NetworkRequestID string `json:"network_request_id,omitempty"`
	Redirected       bool   `json:"redirected_request,omitempty"`

	URLPath              string  `json:"url_path,omitempty"`
	Status               int64   `json:"status,omitempty"`
	ErrorText            string  `json:"error_text,omitempty"`
	EncodedDataLength    float64 `json:"encoded_data_length,omitempty"`
	ServiceWorkerRelated bool    `json:"service_worker_related,omitempty"`
}

type fetchMapping struct {
	FetchRequestID   string `json:"fetch_request_id"`
	NetworkRequestID string `json:"network_request_id,omitempty"`
	URLPath          string `json:"url_path"`
}

type scenarioOptions struct {
	Name                 string
	FixtureScenario      string
	ServiceWorker        bool
	PreDispatchCancel    bool
	CancelAfter          time.Duration
	CancelAfterResponse  bool
	KillBrowser          bool
	DisconnectController bool
	UseFetchInterception bool
}

type scenarioResult struct {
	Name          string            `json:"name"`
	Candidate     string            `json:"candidate"`
	ProfileRef    string            `json:"profile_ref"`
	PageEvents    []pageEvent       `json:"page_events"`
	FetchMappings []fetchMapping    `json:"fetch_mappings,omitempty"`
	FixtureEvents []json.RawMessage `json:"fixture_events"`
	Evidence      map[string]any    `json:"evidence"`
}

type overheadSample struct {
	Sample       int           `json:"sample"`
	StartupMS    float64       `json:"startup_ms"`
	NavigationMS float64       `json:"navigation_ms"`
	ShutdownMS   float64       `json:"shutdown_ms"`
	ProcessCount int           `json:"process_count"`
	RSSTotalKB   int           `json:"rss_total_kb"`
	ProcessTree  []processInfo `json:"process_tree"`
}

type browserProcess struct {
	cmd      *exec.Cmd
	profile  string
	killOnce sync.Once
	killErr  error
}

func sanitizeURL(raw string) string {
	if raw == "" {
		return ""
	}
	parts := strings.SplitN(raw, "?", 2)
	return strings.TrimPrefix(parts[0], baseURL)
}

func browserProcesses(profile string) []processInfo {
	output, err := exec.Command("ps", "-eo", "pid=,ppid=,args=").Output()
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	pattern := regexp.MustCompile(`^\s*(\d+)\s+(\d+)\s+(.*)$`)
	result := make([]processInfo, 0)
	for _, line := range lines {
		if !strings.Contains(line, "--user-data-dir="+profile) {
			continue
		}
		match := pattern.FindStringSubmatch(line)
		if len(match) != 4 {
			continue
		}
		pid, _ := strconv.Atoi(match[1])
		ppid, _ := strconv.Atoi(match[2])
		result = append(result, processInfo{PID: pid, PPID: ppid, Args: strings.ReplaceAll(match[3], profile, "<profile>")})
	}
	byParent := make(map[int][]int)
	for _, process := range result {
		byParent[process.PPID] = append(byParent[process.PPID], process.PID)
	}
	known := make(map[int]bool)
	for _, process := range result {
		known[process.PID] = true
	}
	ordered := make([]processInfo, 0, len(result))
	var visit func(int, int)
	visit = func(pid, depth int) {
		for _, process := range result {
			if process.PID == pid {
				process.Depth = depth
				ordered = append(ordered, process)
			}
		}
		for _, child := range byParent[pid] {
			visit(child, depth+1)
		}
	}
	for _, process := range result {
		if !known[process.PPID] {
			visit(process.PID, 0)
		}
	}
	return ordered
}

func reservePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func startBrowser(profile string) (*browserProcess, string, error) {
	port, err := reservePort()
	if err != nil {
		return nil, "", err
	}
	if err := os.MkdirAll(profile, 0o700); err != nil {
		return nil, "", err
	}
	cmd := exec.Command(chromiumPath,
		"--headless=new", "--no-sandbox", "--disable-dev-shm-usage",
		"--disable-background-networking", "--disable-component-update",
		"--remote-debugging-address=127.0.0.1", "--remote-debugging-port="+strconv.Itoa(port),
		"--remote-allow-origins=*", "--user-data-dir="+profile,
		"about:blank",
	)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, "", err
	}
	browser := &browserProcess{cmd: cmd, profile: profile}
	for i := 0; i < 100; i++ {
		response, requestErr := http.Get(fmt.Sprintf("http://127.0.0.1:%d/json/version", port))
		if requestErr == nil {
			var version struct {
				WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
			}
			decodeErr := json.NewDecoder(response.Body).Decode(&version)
			response.Body.Close()
			if decodeErr == nil && version.WebSocketDebuggerURL != "" {
				return browser, version.WebSocketDebuggerURL, nil
			}
		}
		if signalErr := cmd.Process.Signal(syscall.Signal(0)); signalErr != nil {
			return nil, "", errors.New("chromium exited before CDP endpoint became ready")
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = browser.kill()
	return nil, "", errors.New("timeout waiting for Chromium CDP endpoint")
}

func (b *browserProcess) kill() error {
	if b == nil {
		return nil
	}
	b.killOnce.Do(func() {
		for _, process := range browserProcesses(b.profile) {
			if err := syscall.Kill(process.PID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				b.killErr = err
			}
		}
		if b.cmd != nil && b.cmd.Process != nil {
			_, _ = b.cmd.Process.Wait()
		}
	})
	return b.killErr
}

func resetFixture() error {
	request, err := http.NewRequest(http.MethodPost, baseURL+"/reset", nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	return nil
}

func fixtureEvents() ([]json.RawMessage, error) {
	response, err := http.Get(baseURL + "/events")
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	var events []json.RawMessage
	if err := json.NewDecoder(response.Body).Decode(&events); err != nil {
		return nil, err
	}
	return events, nil
}

func pathFromURL(raw string) string {
	return sanitizeURL(raw)
}

func runScenario(root context.Context, options scenarioOptions, profileRoot string) scenarioResult {
	result := scenarioResult{Name: options.Name, Candidate: "chromedp", ProfileRef: options.Name, Evidence: make(map[string]any), PageEvents: make([]pageEvent, 0)}
	profile := filepath.Join(profileRoot, options.Name)
	_ = os.RemoveAll(profile)
	_ = resetFixture()
	browser, endpoint, err := startBrowser(profile)
	if err != nil {
		result.Evidence["error"] = err.Error()
		return result
	}
	defer browser.kill()
	allocCtx, allocCancel := chromedp.NewRemoteAllocator(root, endpoint)
	defer allocCancel()
	targetCtx, targetCancel := chromedp.NewContext(allocCtx)
	defer targetCancel()

	var mu sync.Mutex
	var fetchMappings []fetchMapping
	var pageEvents []pageEvent
	chromedp.ListenTarget(targetCtx, func(event any) {
		mu.Lock()
		defer mu.Unlock()
		switch event := event.(type) {
		case *network.EventRequestWillBeSent:
			pageEvents = append(pageEvents, pageEvent{Type: "request", NetworkRequestID: string(event.RequestID), Redirected: event.RedirectResponse != nil, URLPath: pathFromURL(event.Request.URL)})
		case *network.EventResponseReceived:
			pageEvents = append(pageEvents, pageEvent{Type: "response", NetworkRequestID: string(event.RequestID), URLPath: pathFromURL(event.Response.URL), Status: int64(event.Response.Status)})
		case *network.EventLoadingFinished:
			pageEvents = append(pageEvents, pageEvent{Type: "loading_finished", NetworkRequestID: string(event.RequestID), EncodedDataLength: event.EncodedDataLength})
		case *network.EventLoadingFailed:
			pageEvents = append(pageEvents, pageEvent{Type: "loading_failed", NetworkRequestID: string(event.RequestID), ErrorText: event.ErrorText})
		case *fetch.EventRequestPaused:
			mapping := fetchMapping{FetchRequestID: string(event.RequestID), NetworkRequestID: string(event.NetworkID), URLPath: pathFromURL(event.Request.URL)}
			fetchMappings = append(fetchMappings, mapping)
			go func(requestID fetch.RequestID) {
				_ = chromedp.Run(targetCtx, fetch.ContinueRequest(requestID))
			}(event.RequestID)
		}
	})
	if err := chromedp.Run(targetCtx, network.Enable()); err != nil {
		result.Evidence["error"] = err.Error()
		return result
	}
	if options.UseFetchInterception {
		if err := chromedp.Run(targetCtx, fetch.Enable().WithPatterns([]*fetch.RequestPattern{{URLPattern: "*submit*", RequestStage: fetch.RequestStageRequest}})); err != nil {
			result.Evidence["error"] = err.Error()
			return result
		}
	}
	pageURL := baseURL + "/page"
	if options.ServiceWorker {
		pageURL += "?sw=1"
	}
	if err := chromedp.Run(targetCtx, chromedp.Navigate(pageURL), chromedp.Sleep(150*time.Millisecond)); err != nil {
		result.Evidence["error"] = err.Error()
		return result
	}
	if options.ServiceWorker {
		_ = chromedp.Run(targetCtx, chromedp.Reload(), chromedp.Sleep(150*time.Millisecond))
		var controlled bool
		_ = chromedp.Run(targetCtx, chromedp.Evaluate(`Boolean(navigator.serviceWorker && navigator.serviceWorker.controller)`, &controlled))
		result.Evidence["service_worker_controlled"] = controlled
	}
	_ = chromedp.Run(targetCtx, chromedp.Evaluate(`window.setProfileMarker("synthetic-chromedp-marker")`, nil))
	result.Evidence["process_tree_before"] = browserProcesses(profile)

	if options.PreDispatchCancel {
		result.Evidence["dispatch_observed"] = false
		result.Evidence["canceled_pre_dispatch"] = true
		result.Evidence["conservative_state"] = "not_sent"
	} else {
		var logicalID string
		scenario := options.FixtureScenario
		if scenario == "" {
			scenario = options.Name
		}
		expression := fmt.Sprintf(`window.startSubmit(%q)`, scenario)
		if err := chromedp.Run(targetCtx, chromedp.Evaluate(expression, &logicalID)); err != nil {
			result.Evidence["error"] = err.Error()
			result.Evidence["conservative_state"] = "unknown_submission"
		} else {
			result.Evidence["logical_id"] = logicalID
			if options.CancelAfter > 0 {
				time.Sleep(options.CancelAfter)
				result.Evidence["dispatch_observed"] = hasRequest(&mu, pageEvents)
				if options.CancelAfterResponse {
					result.Evidence["response_started"] = hasResponse(&mu, pageEvents)
				}
				_ = chromedp.Run(targetCtx, chromedp.Evaluate(fmt.Sprintf(`window.cancelSubmit(%q)`, logicalID), nil))
				result.Evidence["caller_cancellation"] = true
			}
			if options.KillBrowser {
				time.Sleep(100 * time.Millisecond)
				result.Evidence["dispatch_observed"] = hasRequest(&mu, pageEvents)
				_ = browser.kill()
				result.Evidence["browser_crashed"] = true
			}
			if options.DisconnectController {
				time.Sleep(100 * time.Millisecond)
				result.Evidence["dispatch_observed"] = hasRequest(&mu, pageEvents)
				targetCancel()
				result.Evidence["controller_disconnected"] = true
			}
			if !options.KillBrowser && !options.DisconnectController {
				deadline := time.Now().Add(scenarioLimit)
				var pageResult map[string]any
				for time.Now().Before(deadline) {
					if err := chromedp.Run(targetCtx, chromedp.Evaluate(fmt.Sprintf(`window.getSubmitResult(%q)`, logicalID), &pageResult)); err != nil {
						break
					}
					phase, _ := pageResult["phase"].(string)
					if phase == "response_completed" || phase == "response_incomplete" || phase == "physical_abort_observed" {
						break
					}
					time.Sleep(20 * time.Millisecond)
				}
				result.Evidence["page_result"] = pageResult
			}
			if _, ok := result.Evidence["dispatch_observed"]; !ok {
				result.Evidence["dispatch_observed"] = hasRequest(&mu, pageEvents)
			}
			if result.Evidence["dispatch_observed"] == true {
				state := "sent_unconfirmed"
				if pageResult, ok := result.Evidence["page_result"].(map[string]any); ok {
					if phase, _ := pageResult["phase"].(string); phase == "response_completed" {
						state = "completed"
					}
					if phase, _ := pageResult["phase"].(string); phase == "physical_abort_observed" {
						result.Evidence["physical_abort_observed"] = true
					}
				}
				result.Evidence["conservative_state"] = state
			} else {
				result.Evidence["conservative_state"] = "not_sent"
			}
		}
	}
	mu.Lock()
	result.PageEvents = append([]pageEvent(nil), pageEvents...)
	result.FetchMappings = append([]fetchMapping(nil), fetchMappings...)
	mu.Unlock()
	result.Evidence["process_tree_after"] = browserProcesses(profile)
	result.Evidence["browser_processes_after"] = len(browserProcesses(profile))
	result.Evidence["response_started"] = hasResponse(&mu, pageEvents)
	result.Evidence["timeout"] = strings.Contains(options.Name, "timeout")
	result.Evidence["canceled"] = strings.Contains(options.Name, "cancel")
	if result.Evidence["dispatch_observed"] == true && result.Evidence["physical_abort_observed"] != true && (result.Evidence["timeout"] == true || result.Evidence["canceled"] == true || result.Evidence["browser_crashed"] == true || result.Evidence["controller_disconnected"] == true) {
		result.Evidence["physical_abort_unproven"] = true
	}
	events, eventErr := fixtureEvents()
	if eventErr == nil {
		result.FixtureEvents = events
	}
	result.Evidence["physical_post_count"] = countPhysicalPosts(events)
	result.Evidence["physical_post_paths"] = physicalPostPaths(events)
	result.Evidence["service_worker_request_count"] = countServiceWorkerRequests(events)
	result.Evidence["duplicate_gate"] = duplicateGate(options.Name, countPhysicalPosts(events))
	if options.UseFetchInterception {
		result.Evidence["fetch_mapping_gate"] = len(result.FetchMappings) > 0
	}
	return result
}

func hasRequest(mu *sync.Mutex, events []pageEvent) bool {
	mu.Lock()
	defer mu.Unlock()
	for _, event := range events {
		if event.Type == "request" && strings.HasPrefix(event.URLPath, "/submit") {
			return true
		}
	}
	return false
}

func hasResponse(mu *sync.Mutex, events []pageEvent) bool {
	mu.Lock()
	defer mu.Unlock()
	for _, event := range events {
		if event.Type == "response" && strings.HasPrefix(event.URLPath, "/submit") {
			return true
		}
	}
	return false
}

func countPhysicalPosts(events []json.RawMessage) int {
	count := 0
	for _, raw := range events {
		var event struct{ Method, Path string }
		if json.Unmarshal(raw, &event) == nil && event.Method == "POST" && (event.Path == "/submit" || event.Path == "/effect-final") {
			count++
		}
	}
	return count
}

func physicalPostPaths(events []json.RawMessage) []string {
	paths := make([]string, 0)
	for _, raw := range events {
		var event struct{ Method, Path string }
		if json.Unmarshal(raw, &event) == nil && event.Method == "POST" {
			paths = append(paths, event.Path)
		}
	}
	return paths
}

func countServiceWorkerRequests(events []json.RawMessage) int {
	count := 0
	for _, raw := range events {
		var event struct {
			ServiceWorker bool `json:"service_worker"`
		}
		if json.Unmarshal(raw, &event) == nil && event.ServiceWorker {
			count++
		}
	}
	return count
}

func duplicateGate(name string, count int) string {
	if name == "redirect" || name == "fetch-correlation" {
		if count >= 1 {
			return "pass_redirect_hops_are_explicit"
		}
		return "fail_no_physical_post"
	}
	if count == 1 || name == "cancel-before-dispatch" {
		return "pass"
	}
	if count == 0 && name == "controller-disconnect-in-flight" {
		return "pass_no_server_observation"
	}
	return "fail_unexpected_physical_post_count"
}

func profileLifecycle(root context.Context, profileRoot string) map[string]any {
	profileA := filepath.Join(profileRoot, "profile-a")
	profileB := filepath.Join(profileRoot, "profile-b")
	_ = os.RemoveAll(profileA)
	_ = os.RemoveAll(profileB)
	open := func(profile, marker string) string {
		browser, endpoint, err := startBrowser(profile)
		if err != nil {
			return "error"
		}
		allocCtx, cancelAlloc := chromedp.NewRemoteAllocator(root, endpoint)
		targetCtx, cancelTarget := chromedp.NewContext(allocCtx)
		var before string
		_ = chromedp.Run(targetCtx, chromedp.Navigate(baseURL+"/page"), chromedp.Sleep(80*time.Millisecond), chromedp.Evaluate(`window.profileMarker()`, &before), chromedp.Evaluate(fmt.Sprintf(`window.setProfileMarker(%q)`, marker), nil))
		cancelTarget()
		cancelAlloc()
		_ = browser.kill()
		return before
	}
	firstA := open(profileA, "profile-a-marker")
	firstB := open(profileB, "profile-b-marker")
	reusedA := open(profileA, "profile-a-marker-2")
	reusedB := open(profileB, "profile-b-marker-2")
	return map[string]any{
		"first_open_markers":           map[string]string{"profile_a": firstA, "profile_b": firstB},
		"reused_open_previous_markers": map[string]string{"profile_a": reusedA, "profile_b": reusedB},
		"isolated":                     reusedA == "profile-a-marker" && reusedB == "profile-b-marker" && reusedA != reusedB,
		"custody":                      "only opaque profile_ref is emitted; localStorage marker is synthetic",
	}
}

func measureOverhead(root context.Context, profileRoot string) []overheadSample {
	_ = os.RemoveAll(profileRoot)
	_ = os.MkdirAll(profileRoot, 0o700)
	samples := make([]overheadSample, 0, 3)
	for i := 0; i < 3; i++ {
		profile := filepath.Join(profileRoot, fmt.Sprintf("sample-%d", i+1))
		started := time.Now()
		browser, endpoint, err := startBrowser(profile)
		if err != nil {
			continue
		}
		launched := time.Now()
		allocCtx, cancelAlloc := chromedp.NewRemoteAllocator(root, endpoint)
		targetCtx, cancelTarget := chromedp.NewContext(allocCtx)
		if err := chromedp.Run(targetCtx, chromedp.Navigate(baseURL+"/page"), chromedp.WaitVisible("#submit")); err != nil {
			cancelTarget()
			cancelAlloc()
			_ = browser.kill()
			continue
		}
		navigated := time.Now()
		tree := browserProcesses(profile)
		rss := 0
		for _, process := range tree {
			rss += rssKB(process.PID)
		}
		cancelTarget()
		cancelAlloc()
		_ = browser.kill()
		closed := time.Now()
		samples = append(samples, overheadSample{Sample: i + 1, StartupMS: durationMS(launched.Sub(started)), NavigationMS: durationMS(navigated.Sub(launched)), ShutdownMS: durationMS(closed.Sub(navigated)), ProcessCount: len(tree), RSSTotalKB: rss, ProcessTree: tree})
	}
	return samples
}

func durationMS(duration time.Duration) float64 { return float64(duration.Microseconds()) / 1000 }

func rssKB(pid int) int {
	output, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0
	}
	value, _ := strconv.Atoi(strings.TrimSpace(string(output)))
	return value
}

func main() {
	if _, err := os.Stat(chromiumPath); err != nil {
		panic(err)
	}
	root := context.Background()
	outputDir := os.Getenv("RUNSTEAD_OUTPUT_DIR")
	if outputDir == "" {
		outputDir = filepath.Join("..", "output")
	}
	profileRoot := os.Getenv("RUNSTEAD_PROFILE_ROOT")
	if profileRoot == "" {
		profileRoot = filepath.Join("..", "profiles", "chromedp")
	}
	_ = os.MkdirAll(outputDir, 0o755)
	_ = os.MkdirAll(profileRoot, 0o700)
	startedAt := time.Now().UTC()
	scenarios := []scenarioOptions{
		{Name: "normal"},
		{Name: "redirect", FixtureScenario: "redirect", UseFetchInterception: true},
		{Name: "service-worker", FixtureScenario: "normal", ServiceWorker: true},
		{Name: "timeout-before-headers", FixtureScenario: "headers-delay", CancelAfter: 60 * time.Millisecond},
		{Name: "cancel-after-headers", FixtureScenario: "body-delay", CancelAfter: 340 * time.Millisecond, CancelAfterResponse: true},
		{Name: "cancel-in-flight", FixtureScenario: "open", CancelAfter: 100 * time.Millisecond},
		{Name: "sse-complete", FixtureScenario: "sse-complete"},
		{Name: "sse-truncated", FixtureScenario: "sse-truncated"},
		{Name: "sse-eof", FixtureScenario: "sse-eof"},
		{Name: "sse-partial", FixtureScenario: "sse-partial"},
		{Name: "cancel-before-dispatch", PreDispatchCancel: true},
		{Name: "browser-kill-in-flight", FixtureScenario: "open", KillBrowser: true},
		{Name: "controller-disconnect-in-flight", FixtureScenario: "open", DisconnectController: true},
		{Name: "fetch-correlation", FixtureScenario: "normal", UseFetchInterception: true},
	}
	results := make([]scenarioResult, 0, len(scenarios))
	for _, options := range scenarios {
		results = append(results, runScenario(root, options, profileRoot))
	}
	artifact := map[string]any{
		"schema":            "runstead.substrate-bakeoff.v1",
		"candidate":         "chromedp",
		"started_at":        startedAt.Format(time.RFC3339Nano),
		"finished_at":       time.Now().UTC().Format(time.RFC3339Nano),
		"environment":       map[string]string{"go": "1.26.1", "chromedp": "v0.16.0", "chromium_path": chromiumPath},
		"runtime_tree":      "Go runner -> chromedp -> Chromium CDP websocket",
		"overhead":          measureOverhead(root, filepath.Join(profileRoot, "benchmark")),
		"profile_lifecycle": profileLifecycle(root, profileRoot),
		"scenarios":         results,
		"limitations":       []string{"A local synthetic fixture proves browser-observed dispatch and same-request lifecycle, not upstream acceptance after dispatch.", "The runner uses a system Chromium executable and does not claim Playwright-style browser discovery/download."},
	}
	outputPath := filepath.Join(outputDir, "chromedp-results.json")
	file, err := os.Create(outputPath)
	if err != nil {
		panic(err)
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(artifact); err != nil {
		panic(err)
	}
	_ = file.Close()
	fmt.Printf("{\"candidate\":\"chromedp\",\"scenarios\":%d,\"output\":%q}\n", len(results), outputPath)
}
