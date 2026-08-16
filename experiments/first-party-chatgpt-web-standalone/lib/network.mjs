// Transport-level network accounting, authoritative source for this spike.
// Uses ONLY the CDP Network domain events at the browser level. The
// fetch/XHR wrapper approach of the first spike is NOT used here: nothing is
// injected into the page for counting.
//
// Sanitization is structural and happens at event ingestion: only the
// allowlisted fields below are ever kept. Headers, cookies, bodies,
// credential material, query strings and response data are never read or
// logged. Request ids are truncated; hosts are hostnames only; paths are
// pathnames only (query strings dropped).

import { redact } from "./sanitize.mjs";

const MODEL_EFFECT_PATHS = new Set([
  "/backend-api/conversation",
  "/backend-api/f/conversation",
]);

export function classifyRequest(method, pathname) {
  if (method === "POST" && MODEL_EFFECT_PATHS.has(pathname)) {
    return "model_effect_conversation";
  }
  if (method === "POST" && pathname === "/backend-api/f/conversation/prepare") {
    return "model_effect_prepare"; // pre-dispatch auxiliary (prepare step)
  }
  if (
    pathname === "/backend-api/me" ||
    pathname.startsWith("/backend-api/accounts/") ||
    pathname === "/backend-api/settings/user"
  ) {
    return "session_check";
  }
  if (pathname.startsWith("/backend-api/conversation/")) {
    return "conversation_api_aux"; // stream_status / textdocs / etc.
  }
  if (pathname.startsWith("/backend-api/conversations")) {
    return "conversation_list";
  }
  if (pathname.startsWith("/backend-api/sentinel/")) {
    return "sentinel_aux";
  }
  if (pathname.startsWith("/ces/")) {
    return "ces_telemetry";
  }
  if (
    pathname.startsWith("/_next/") ||
    pathname.startsWith("/static/") ||
    pathname.startsWith("/fonts/") ||
    pathname.startsWith("/assets/")
  ) {
    return "static_asset";
  }
  if (pathname.startsWith("/backend-api/")) {
    return "backend_api_aux";
  }
  return "other";
}

function truncateId(id) {
  if (typeof id !== "string") return "";
  return id.length > 12 ? id.slice(0, 12) + "\u2026" : id;
}

function parseUrl(rawUrl) {
  try {
    const u = new URL(rawUrl);
    return { host: u.hostname, path: u.pathname };
  } catch {
    return { host: "", path: "" };
  }
}

// Long script/asset paths are truncated for evidence compactness.
// Classification always runs on the full path before truncation.
function truncatePath(path) {
  return path.length > 160 ? path.slice(0, 160) + "\u2026" : path;
}

export class NetworkObserver {
  constructor(cdp, sessionId, onRecord) {
    this.cdp = cdp;
    this.sessionId = sessionId;
    this.onRecord = onRecord;
    this.records = [];
    this.requests = new Map(); // requestId -> sanitized state
    this.seq = 0;
    this.window = "baseline";
    this.turnId = 0; // incremented per openTurn(); scopes per-turn queries
    this.baselineStartedAt = null;
    this.turnOpenedAt = null;
    this.turnClosedAt = null;
  }

  async enable() {
    await this.cdp.send("Network.enable", {}, this.sessionId);
    this.cdp.onEvent((method, params, sessionId) => {
      if (sessionId !== this.sessionId) return;
      this._handle(method, params);
    });
  }

  openBaseline() {
    this.window = "baseline";
    this.baselineStartedAt = Date.now();
  }

  openTurn() {
    this.window = "turn";
    this.turnId += 1;
    this.turnOpenedAt = Date.now();
    this.turnClosedAt = null;
    return this.turnId;
  }

  closeTurn() {
    this.window = "post_turn";
    this.turnClosedAt = Date.now();
  }

  _record(kind, fields) {
    const rec = redact({
      seq: ++this.seq,
      kind,
      ts: Date.now(),
      window: this.window,
      turn: this.turnId,
      ...fields,
    });
    this.records.push(rec);
    if (this.onRecord) this.onRecord(rec);
  }

  _handle(method, params) {
    switch (method) {
      case "Network.requestWillBeSent": {
        const { host, path: fullPath } = parseUrl(params.request?.url || "");
        const path = truncatePath(fullPath);
        const state = {
          requestId: truncateId(params.requestId),
          method: params.request?.method || "",
          host,
          path,
          type: params.type || "",
          classification: classifyRequest(params.request?.method || "", fullPath),
          window: this.window,
          turn: this.turnId,
          requestedAt: Date.now(),
          status: null,
          mimeType: "",
          fromCache: false,
          completion: null, // "finished" | "failed"
          errorText: null,
          streamChunks: 0,
          streamBytes: 0,
        };
        this.requests.set(params.requestId, state);
        this._record("request_will_be_sent", {
          requestId: state.requestId,
          method: state.method,
          host: state.host,
          path: state.path,
          type: state.type,
          classification: state.classification,
        });
        break;
      }
      case "Network.responseReceived": {
        const state = this.requests.get(params.requestId);
        if (!state) return;
        state.status = params.response?.status ?? null;
        state.mimeType = params.response?.mimeType ?? "";
        state.fromCache = !!params.response?.fromDiskCache;
        this._record("response_received", {
          requestId: state.requestId,
          classification: state.classification,
          status: state.status,
          mimeType: state.mimeType,
        });
        break;
      }
      case "Network.dataReceived": {
        const state = this.requests.get(params.requestId);
        if (!state) return;
        state.streamChunks += 1;
        state.streamBytes += params.dataLength ?? 0;
        this._record("data_received", {
          requestId: state.requestId,
          classification: state.classification,
          streamChunks: state.streamChunks,
        });
        break;
      }
      case "Network.loadingFinished": {
        const state = this.requests.get(params.requestId);
        if (!state) return;
        state.completion = "finished";
        this._record("loading_finished", {
          requestId: state.requestId,
          classification: state.classification,
          streamBytes: state.streamBytes,
        });
        break;
      }
      case "Network.loadingFailed": {
        const state = this.requests.get(params.requestId);
        if (!state) return;
        state.completion = "failed";
        state.errorText = params.errorText ?? "";
        this._record("loading_failed", {
          requestId: state.requestId,
          classification: state.classification,
          errorText: state.errorText,
          canceled: !!params.canceled,
          type: params.type ?? "",
        });
        break;
      }
      default:
        break;
    }
  }

  // --- queries over recorded state -------------------------------------

  requestsInWindow(windowName, turnId = null) {
    return this.records.filter(
      (r) =>
        r.kind === "request_will_be_sent" &&
        r.window === windowName &&
        (turnId === null || r.turn === turnId)
    );
  }

  countByClassification(windowName, turnId = null) {
    const counts = {};
    for (const r of this.requestsInWindow(windowName, turnId)) {
      counts[r.classification] = (counts[r.classification] || 0) + 1;
    }
    return counts;
  }

  modelEffectRequests(windowName, turnId = null) {
    return this.requestsInWindow(windowName, turnId).filter(
      (r) => r.classification === "model_effect_conversation"
    );
  }

  // Transport-level streaming-start evidence for a model-effect request:
  // first dataReceived chunk on the SSE conversation request.
  conversationResponseStarted(requestIdPrefix) {
    const reqs = this.requests.values();
    for (const r of reqs) {
      if (
        r.classification === "model_effect_conversation" &&
        r.requestId.startsWith(requestIdPrefix) &&
        r.streamChunks > 0
      ) {
        return true;
      }
    }
    return false;
  }

  // Current turn's model-effect requests, scoped by turn id.
  turnConversationRequests(turnId) {
    const out = [];
    for (const r of this.requests.values()) {
      if (
        r.classification === "model_effect_conversation" &&
        r.turn === turnId
      ) {
        out.push(r);
      }
    }
    return out;
  }

  conversationRequests() {
    const out = [];
    for (const r of this.requests.values()) {
      if (r.classification === "model_effect_conversation") out.push(r);
    }
    return out;
  }
}
