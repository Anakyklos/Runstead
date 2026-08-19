package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

type requestEvent struct {
	Index         int       `json:"index"`
	ReceivedAt    time.Time `json:"received_at"`
	Method        string    `json:"method"`
	Path          string    `json:"path"`
	Scenario      string    `json:"scenario"`
	BodySHA256    string    `json:"body_sha256"`
	BodyBytes     int       `json:"body_bytes"`
	ServiceWorker bool      `json:"service_worker"`
	Outcome       string    `json:"outcome"`
	Canceled      bool      `json:"canceled"`
}

type fixture struct {
	mu     sync.Mutex
	events []requestEvent
}

func (f *fixture) appendEvent(r *http.Request, scenario, outcome string, body []byte) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	hash := sha256.Sum256(body)
	index := len(f.events) + 1
	f.events = append(f.events, requestEvent{
		Index:         index,
		ReceivedAt:    time.Now().UTC(),
		Method:        r.Method,
		Path:          r.URL.Path,
		Scenario:      scenario,
		BodySHA256:    hex.EncodeToString(hash[:]),
		BodyBytes:     len(body),
		ServiceWorker: r.Header.Get("X-SW-Intercepted") == "1",
		Outcome:       outcome,
	})
	return index
}

func (f *fixture) reset(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	f.events = nil
	f.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (f *fixture) eventsHandler(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	events := f.events
	if events == nil {
		events = []requestEvent{}
	}
	_ = json.NewEncoder(w).Encode(events)
}

func (f *fixture) page(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, `<!doctype html>
<html><head><meta charset="utf-8"><title>Runstead substrate fixture</title></head>
<body><main><h1>synthetic substrate fixture</h1><button id="submit">submit</button><output id="status"></output></main>
<script>
(() => {
  let sequence = 0;
  const active = new Map();
  const results = new Map();
  window.profileMarker = () => localStorage.getItem('runstead-profile-marker');
  window.setProfileMarker = value => localStorage.setItem('runstead-profile-marker', value);
  window.startSubmit = scenario => {
    const id = 'logical-' + (++sequence);
    const controller = new AbortController();
    active.set(id, controller);
    results.set(id, {id, scenario, phase: 'pre_dispatch'});
    fetch('/submit?scenario=' + encodeURIComponent(scenario), {
      method: 'POST',
      body: JSON.stringify({logical_id: id, synthetic_payload: 'runstead-bakeoff'}),
      headers: {'content-type': 'application/json'},
      signal: controller.signal,
    }).then(async response => {
      const state = results.get(id) || {id, scenario};
      state.phase = 'response_started';
      state.status = response.status;
      state.redirected = response.redirected;
      state.url = new URL(response.url).pathname;
      const text = await response.text();
      state.body_bytes = text.length;
      state.terminal = text.includes('data: [DONE]');
      state.phase = state.terminal ? 'response_completed' : 'response_incomplete';
      results.set(id, state);
    }).catch(error => {
      const state = results.get(id) || {id, scenario};
      state.phase = error && error.name === 'AbortError' ? 'physical_abort_observed' : 'response_incomplete';
      state.error_name = error && error.name ? error.name : 'unknown';
      state.error_message = String(error && error.message ? error.message : error);
      results.set(id, state);
    }).finally(() => active.delete(id));
    return id;
  };
  window.cancelSubmit = id => {
    const controller = active.get(id) || active.values().next().value;
    if (controller) controller.abort();
  };
  window.getSubmitResult = id => results.get(id) || null;
  window.serviceWorkerControlled = () => Boolean(navigator.serviceWorker && navigator.serviceWorker.controller);
  if (new URLSearchParams(location.search).get('sw') === '1' && navigator.serviceWorker) {
    navigator.serviceWorker.register('/sw.js').catch(() => {});
  }
})();
</script></body></html>`)
}

func sw(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/javascript")
	_, _ = io.WriteString(w, `self.addEventListener('fetch', event => {
  const url = new URL(event.request.url);
  if (url.pathname !== '/submit') return;
  const headers = new Headers(event.request.headers);
  headers.set('x-sw-intercepted', '1');
  event.respondWith(fetch(new Request(event.request, {headers})));
});`)
}

func (f *fixture) submit(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	scenario := r.URL.Query().Get("scenario")
	if scenario == "" {
		scenario = "normal"
	}

	if r.URL.Path == "/submit" && scenario == "redirect" {
		f.appendEvent(r, scenario, "redirect_hop", body)
		w.Header().Set("Location", "/effect-final?scenario=redirect")
		w.WriteHeader(http.StatusTemporaryRedirect)
		return
	}

	index := f.appendEvent(r, scenario, "received", body)
	if scenario == "headers-delay" {
		time.Sleep(300 * time.Millisecond)
	}
	if scenario == "open" {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			_, _ = fmt.Fprintf(w, "request=%d\n", index)
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
			f.mu.Lock()
			if len(f.events) >= index {
				f.events[index-1].Canceled = true
				f.events[index-1].Outcome = "client_disconnected"
			}
			f.mu.Unlock()
		case <-time.After(5 * time.Second):
		}
		return
	}
	if scenario == "body-delay" {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			_, _ = fmt.Fprint(w, "headers-sent\n")
			flusher.Flush()
		}
		time.Sleep(300 * time.Millisecond)
		_, _ = fmt.Fprint(w, "body-after-delay\n")
		return
	}
	if scenario == "sse-complete" || scenario == "sse-truncated" || scenario == "sse-eof" || scenario == "sse-partial" {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = fmt.Fprintf(w, "data: {\"request_index\":%d,\"text\":\"synthetic\"}\n\n", index)
		if flusher != nil {
			flusher.Flush()
		}
		if scenario == "sse-complete" {
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Fixture-Request-Index", strconv.Itoa(index))
	_, _ = fmt.Fprintf(w, `{"ok":true,"request_index":%d,"scenario":%q}`, index, scenario)
}

func main() {
	addr := os.Getenv("RUNSTEAD_FIXTURE_ADDR")
	if addr == "" {
		addr = "127.0.0.1:18765"
	}
	f := &fixture{}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	mux.HandleFunc("/reset", f.reset)
	mux.HandleFunc("/events", f.eventsHandler)
	mux.HandleFunc("/page", f.page)
	mux.HandleFunc("/sw.js", sw)
	mux.HandleFunc("/submit", f.submit)
	mux.HandleFunc("/effect-final", f.submit)
	server := &http.Server{Addr: addr, Handler: mux}
	log.Printf("fixture listening on %s", addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
