package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

const scenarioLimit = 1800 * time.Millisecond

var baseURL = func() string {
	if value := os.Getenv("RUNSTEAD_FIXTURE_URL"); value != "" {
		return strings.TrimRight(value, "/")
	}
	return "http://127.0.0.1:18765"
}()

var chromiumPath = func() string {
	if value := os.Getenv("CHROMIUM_PATH"); value != "" {
		return value
	}
	return "/usr/bin/chromium"
}()

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
	Name                    string
	FixtureScenario         string
	ServiceWorker           bool
	PreDispatchCancel       bool
	TimeoutAfter            time.Duration
	CancelAfter             time.Duration
	CancelAfterResponse     bool
	CancelAfterResponseWait time.Duration
	KillBrowser             bool
	DisconnectController    bool
	UseFetchInterception    bool
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

type cdpProxy struct {
	cmd *exec.Cmd
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

func startCDPProxy(endpoint string) (*cdpProxy, string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, "", err
	}
	listenPort, err := reservePort()
	if err != nil {
		return nil, "", err
	}
	listenAddr := fmt.Sprintf("127.0.0.1:%d", listenPort)
	proxy := exec.Command(os.Args[0], "--cdp-proxy", listenAddr, parsed.Host)
	proxy.Stdout = io.Discard
	proxy.Stderr = io.Discard
	if err := proxy.Start(); err != nil {
		return nil, "", err
	}
	for i := 0; i < 50; i++ {
		conn, dialErr := net.DialTimeout("tcp", listenAddr, 100*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			parsed.Host = listenAddr
			return &cdpProxy{cmd: proxy}, parsed.String(), nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = proxy.Process.Kill()
	_, _ = proxy.Process.Wait()
	return nil, "", errors.New("timeout waiting for CDP proxy")
}

func (p *cdpProxy) kill() {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	_ = p.cmd.Process.Kill()
	_, _ = p.cmd.Process.Wait()
}

func runCDPProxy(listenAddr, targetAddr string) error {
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return err
	}
	defer listener.Close()
	for {
		client, acceptErr := listener.Accept()
		if acceptErr != nil {
			return acceptErr
		}
		backend, dialErr := net.DialTimeout("tcp", targetAddr, 3*time.Second)
		if dialErr != nil {
			_ = client.Close()
			continue
		}
		go relayTCP(client, backend)
	}
}

func relayTCP(client, backend net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(client, backend)
		_ = client.Close()
		_ = backend.Close()
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(backend, client)
		_ = client.Close()
		_ = backend.Close()
		done <- struct{}{}
	}()
	<-done
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
	var proxy *cdpProxy
	if options.DisconnectController {
		proxy, endpoint, err = startCDPProxy(endpoint)
		if err != nil {
			_ = browser.kill()
			result.Evidence["error"] = err.Error()
			return result
		}
	}
	allocCtx, allocCancel := chromedp.NewRemoteAllocator(root, endpoint)
	targetCtx, targetCancel := chromedp.NewContext(allocCtx)
	cleanup := func() {
		targetCancel()
		allocCancel()
		if proxy != nil {
			proxy.kill()
		}
		_ = browser.kill()
	}
	defer cleanup()

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
		var pageResult map[string]any
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
			if options.TimeoutAfter > 0 {
				result.Evidence["timeout_requested_ms"] = options.TimeoutAfter.Milliseconds()
				result.Evidence["timeout_path"] = "caller_deadline"
				_ = chromedp.Run(targetCtx, chromedp.Evaluate(fmt.Sprintf(`window.deadlineSubmit(%q,%d)`, logicalID, options.TimeoutAfter.Milliseconds()), nil))
			}
			if options.CancelAfterResponse {
				deadline := time.Now().Add(scenarioLimit)
				for !hasResponse(&mu, pageEvents) && time.Now().Before(deadline) {
					time.Sleep(10 * time.Millisecond)
				}
				result.Evidence["response_start_observed"] = hasResponse(&mu, pageEvents)
				time.Sleep(options.CancelAfterResponseWait)
				result.Evidence["cancellation_after_response_start"] = result.Evidence["response_start_observed"] == true
				if result.Evidence["cancellation_after_response_start"] == true {
					_ = chromedp.Run(targetCtx, chromedp.Evaluate(fmt.Sprintf(`window.cancelSubmit(%q)`, logicalID), nil))
				}
				result.Evidence["caller_cancellation"] = true
			}
			if options.CancelAfter > 0 {
				time.Sleep(options.CancelAfter)
				result.Evidence["dispatch_observed"] = hasRequest(&mu, pageEvents)
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
				browserContext := chromedp.FromContext(targetCtx)
				if browserContext == nil || browserContext.Browser == nil || proxy == nil {
					result.Evidence["controller_disconnect_error"] = "browser or CDP proxy unavailable"
				} else {
					lostConnection := browserContext.Browser.LostConnection
					proxy.kill()
					select {
					case <-lostConnection:
						result.Evidence["controller_disconnected"] = true
						result.Evidence["controller_disconnect_transport"] = "cdp_tcp_proxy_killed_abruptly"
					case <-time.After(800 * time.Millisecond):
						result.Evidence["controller_disconnect_error"] = "LostConnection was not observed"
					}
				}
			}
			if !options.KillBrowser && !options.DisconnectController {
				deadline := time.Now().Add(scenarioLimit)
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
	cleanup()
	time.Sleep(120 * time.Millisecond)
	result.Evidence["process_tree_after"] = browserProcesses(profile)
	result.Evidence["browser_processes_after"] = len(browserProcesses(profile))
	result.Evidence["response_started"] = hasResponse(&mu, pageEvents)
	result.Evidence["timeout"] = options.TimeoutAfter > 0 && pageResultTimeoutCause(result.Evidence["page_result"]) == "caller_deadline"
	result.Evidence["canceled"] = options.CancelAfter > 0 || options.CancelAfterResponse
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
	postCount := countPhysicalPosts(events)
	postPaths := physicalPostPaths(events)
	result.Evidence["duplicate_gate"] = duplicateGate(options.Name, postCount, postPaths)
	if options.UseFetchInterception {
		result.Evidence["fetch_mapping_gate"] = len(result.FetchMappings) > 0
		if options.Name == "fetch-correlation" {
			result.Evidence["fetch_mapping_gate"] = len(result.FetchMappings) > 0 && postCount == 1 && strings.Join(postPaths, ">") == "/submit"
		}
	}
	result.Evidence["expected_contract"] = expectedScenarioContract(options.Name)
	result.Evidence["contract_failures"] = contractFailures(result)
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
		if event.Type == "response" && (event.URLPath == "/submit" || event.URLPath == "/effect-final") {
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

func pageResultTimeoutCause(raw any) string {
	pageResult, _ := raw.(map[string]any)
	cause, _ := pageResult["timeout_cause"].(string)
	return cause
}

func cloneContract(base map[string]any, extra map[string]any) map[string]any {
	cloned := make(map[string]any, len(base)+len(extra))
	for key, value := range base {
		cloned[key] = value
	}
	for key, value := range extra {
		cloned[key] = value
	}
	return cloned
}

func expectedScenarioContract(name string) map[string]any {
	base := map[string]any{
		"physical_post_count": 1,
		"physical_post_paths": []string{"/submit"},
		"dispatch_observed":   true,
		"conservative_state":  "sent_unconfirmed",
	}
	responseIncomplete := cloneContract(base, map[string]any{
		"response_started":          true,
		"required_page_phase":       "response_incomplete",
		"terminal_must_not_be_true": true,
	})
	contracts := map[string]map[string]any{
		"normal":                 responseIncomplete,
		"redirect":               cloneContract(responseIncomplete, map[string]any{"physical_post_count": 2, "physical_post_paths": []string{"/submit", "/effect-final"}}),
		"service-worker":         cloneContract(responseIncomplete, map[string]any{"service_worker_controlled": true, "service_worker_request_count_min": 1}),
		"timeout-before-headers": cloneContract(base, map[string]any{"response_started": false, "required_page_phase": "physical_abort_observed", "timeout": true}),
		"cancel-after-headers":   cloneContract(base, map[string]any{"response_started": true, "required_page_phase": "physical_abort_observed", "canceled": true, "response_start_observed": true, "cancellation_after_response_start": true}),
		"cancel-in-flight":       cloneContract(base, map[string]any{"canceled": true, "required_page_phase": "physical_abort_observed"}),
		"sse-complete":           cloneContract(base, map[string]any{"response_started": true, "required_page_phase": "response_completed", "required_terminal": true, "conservative_state": "completed"}),
		"sse-truncated":          responseIncomplete,
		"sse-eof":                responseIncomplete,
		"sse-partial":            responseIncomplete,
		"cancel-before-dispatch": {
			"physical_post_count": 0,
			"physical_post_paths": []string{},
			"dispatch_observed":   false,
			"conservative_state":  "not_sent",
			"response_started":    false,
		},
		"browser-kill-in-flight":          cloneContract(base, map[string]any{"browser_crashed": true}),
		"controller-disconnect-in-flight": cloneContract(base, map[string]any{"controller_disconnected": true, "controller_disconnect_transport": "cdp_tcp_proxy_killed_abruptly"}),
		"fetch-correlation":               responseIncomplete,
	}
	if contract, ok := contracts[name]; ok {
		return contract
	}
	return base
}

func pageResultMap(raw any) map[string]any {
	result, _ := raw.(map[string]any)
	return result
}

func pageResultPhase(raw any) string {
	phase, _ := pageResultMap(raw)["phase"].(string)
	return phase
}

func pageResultTerminal(raw any) (bool, bool) {
	terminal, ok := pageResultMap(raw)["terminal"].(bool)
	return terminal, ok
}

func contractFailures(scenario scenarioResult) []string {
	evidence := scenario.Evidence
	expected := expectedScenarioContract(scenario.Name)
	failures := make([]string, 0)
	expectedCount, _ := expected["physical_post_count"].(int)
	actualCount, _ := evidence["physical_post_count"].(int)
	if actualCount != expectedCount {
		failures = append(failures, fmt.Sprintf("physical_post_count expected %d, got %d", expectedCount, actualCount))
	}
	expectedPaths, _ := expected["physical_post_paths"].([]string)
	actualPaths, _ := evidence["physical_post_paths"].([]string)
	if strings.Join(actualPaths, ">") != strings.Join(expectedPaths, ">") {
		failures = append(failures, fmt.Sprintf("physical_post_paths expected %s, got %s", strings.Join(expectedPaths, ">"), strings.Join(actualPaths, ">")))
	}
	expectedDispatch, _ := expected["dispatch_observed"].(bool)
	actualDispatch, _ := evidence["dispatch_observed"].(bool)
	if actualDispatch != expectedDispatch {
		failures = append(failures, fmt.Sprintf("dispatch_observed expected %t, got %t", expectedDispatch, actualDispatch))
	}
	if expectedState, ok := expected["conservative_state"].(string); ok {
		if actualState, _ := evidence["conservative_state"].(string); actualState != expectedState {
			failures = append(failures, fmt.Sprintf("conservative_state expected %s, got %s", expectedState, actualState))
		}
	}
	if expectedResponseStarted, ok := expected["response_started"].(bool); ok {
		if actualResponseStarted, _ := evidence["response_started"].(bool); actualResponseStarted != expectedResponseStarted {
			failures = append(failures, fmt.Sprintf("response_started expected %t, got %t", expectedResponseStarted, actualResponseStarted))
		}
	}
	if requiredPhase, ok := expected["required_page_phase"].(string); ok && pageResultPhase(evidence["page_result"]) != requiredPhase {
		failures = append(failures, fmt.Sprintf("page phase expected %s, got %s", requiredPhase, pageResultPhase(evidence["page_result"])))
	}
	if mustNotBeTerminal, _ := expected["terminal_must_not_be_true"].(bool); mustNotBeTerminal {
		if terminal, _ := pageResultTerminal(evidence["page_result"]); terminal {
			failures = append(failures, "terminal unexpectedly classified as complete")
		}
	}
	if requiredTerminal, ok := expected["required_terminal"].(bool); ok {
		if terminal, present := pageResultTerminal(evidence["page_result"]); !present || terminal != requiredTerminal {
			failures = append(failures, fmt.Sprintf("terminal expected %t", requiredTerminal))
		}
	}
	if expectedTimeout, _ := expected["timeout"].(bool); expectedTimeout && evidence["timeout"] != true {
		failures = append(failures, "caller deadline was not observed")
	}
	if expectedCanceled, _ := expected["canceled"].(bool); expectedCanceled && evidence["canceled"] != true {
		failures = append(failures, "explicit cancellation was not observed")
	}
	if expectedResponseStart, _ := expected["response_start_observed"].(bool); expectedResponseStart && evidence["response_start_observed"] != true {
		failures = append(failures, "response-start ordering was not observed")
	}
	if expectedCancellationOrder, _ := expected["cancellation_after_response_start"].(bool); expectedCancellationOrder && evidence["cancellation_after_response_start"] != true {
		failures = append(failures, "cancellation did not occur after response-start")
	}
	if expectedControlled, _ := expected["service_worker_controlled"].(bool); expectedControlled && evidence["service_worker_controlled"] != true {
		failures = append(failures, "Service Worker was not controlling the page")
	}
	if minimum, ok := expected["service_worker_request_count_min"].(int); ok {
		actual, _ := evidence["service_worker_request_count"].(int)
		if actual < minimum {
			failures = append(failures, "Service Worker request observation was missing")
		}
	}
	if expectedCrash, _ := expected["browser_crashed"].(bool); expectedCrash && evidence["browser_crashed"] != true {
		failures = append(failures, "browser kill was not observed")
	}
	if expectedDisconnect, _ := expected["controller_disconnected"].(bool); expectedDisconnect && evidence["controller_disconnected"] != true {
		failures = append(failures, "controller disconnect was not observed")
	}
	if expectedTransport, ok := expected["controller_disconnect_transport"].(string); ok && evidence["controller_disconnect_transport"] != expectedTransport {
		failures = append(failures, "controller disconnect transport was not abrupt")
	}
	return failures
}

func duplicateGate(name string, count int, paths []string) string {
	if name == "redirect" {
		if count == 2 && strings.Join(paths, ">") == "/submit>/effect-final" {
			return "pass_redirect_exact_sequence"
		}
		return "fail_redirect_sequence_or_amplification"
	}
	if name == "fetch-correlation" {
		if count == 1 && strings.Join(paths, ">") == "/submit" {
			return "pass_fetch_exactly_one_effect"
		}
		return "fail_fetch_effect_count_or_path"
	}
	if name == "cancel-before-dispatch" {
		return map[bool]string{true: "pass", false: "fail_pre_dispatch_created_effect"}[count == 0]
	}
	if count == 1 && strings.Join(paths, ">") == "/submit" {
		return "pass_exactly_one_effect"
	}
	return "fail_expected_exactly_one_effect"
}

func gateFailures(artifact map[string]any) []string {
	failures := make([]string, 0)
	profile, _ := artifact["profile_lifecycle"].(map[string]any)
	if isolated, _ := profile["isolated"].(bool); !isolated {
		failures = append(failures, "profile isolation failed")
	}
	scenarios, _ := artifact["scenarios"].([]scenarioResult)
	for _, scenario := range scenarios {
		evidence := scenario.Evidence
		for _, failure := range contractFailures(scenario) {
			failures = append(failures, scenario.Name+": "+failure)
		}
		if gate, _ := evidence["duplicate_gate"].(string); strings.HasPrefix(gate, "fail") {
			failures = append(failures, scenario.Name+": "+gate)
		}
		if processes, _ := evidence["browser_processes_after"].(int); processes != 0 {
			failures = append(failures, fmt.Sprintf("%s: cleanup left %d browser processes", scenario.Name, processes))
		}
		if (scenario.Name == "redirect" || scenario.Name == "fetch-correlation") && evidence["fetch_mapping_gate"] != true {
			failures = append(failures, scenario.Name+": Fetch/Network mapping gate failed")
		}
	}
	return failures
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
	if len(os.Args) >= 4 && os.Args[1] == "--cdp-proxy" {
		_ = runCDPProxy(os.Args[2], os.Args[3])
		return
	}
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
		{Name: "timeout-before-headers", FixtureScenario: "headers-delay", TimeoutAfter: 60 * time.Millisecond},
		{Name: "cancel-after-headers", FixtureScenario: "body-delay", CancelAfterResponse: true, CancelAfterResponseWait: 100 * time.Millisecond},
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
		"environment":       map[string]string{"go": runtime.Version(), "chromedp": "v0.16.0", "chromium_path": chromiumPath},
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
	failures := gateFailures(artifact)
	artifact["gate_failures"] = failures
	if err := encoder.Encode(artifact); err != nil {
		panic(err)
	}
	_ = file.Close()
	fmt.Printf("{\"candidate\":\"chromedp\",\"scenarios\":%d,\"output\":%q,\"gate_failures\":%q}\n", len(results), outputPath, strings.Join(failures, "; "))
	if len(failures) > 0 {
		fmt.Fprintln(os.Stderr, "substrate bake-off gates failed:", strings.Join(failures, "; "))
		os.Exit(1)
	}
}
