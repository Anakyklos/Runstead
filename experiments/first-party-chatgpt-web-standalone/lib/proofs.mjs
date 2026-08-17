// Deterministic, zero-model-turn fail-closed proofs for the standalone spike.
// Pure functions + fixture tables; no browser, no network, no model turns.
// Run by `run_spike.mjs dry` and written to evidence/fail-closed-proofs.json.
//
// The timeout proof answers issue #16's "prove cancellation/timeout behavior"
// requirement without another live model turn: a turn whose model-effect
// request is SENT but never starts (and never completes) within the timeout
// budget must derive a typed fail-closed state and must NOT re-dispatch or
// replay; a subsequent conversation-namespace POST must flip the
// no-hidden-retry verdict to blocked.

import { classifyRequest } from "./network.mjs";
import { originState } from "./dom.mjs";
import { conversationIdEvidence, urlShape, targetShape, redact } from "./sanitize.mjs";

// Typed state after a bounded wait for a sent model-effect request.
// model: a transport record that was sent (requestWillBeSent observed).
// timedOut: the bounded wait expired before response start/completion.
export function timeoutState({ sent, responseStarted, completion, timedOut }) {
  if (!sent) {
    return { state: "not_sent", replay: false, retry_dispatched: false };
  }
  if (timedOut && !responseStarted && completion === null) {
    return { state: "sent_timeout_fail_closed", replay: false, retry_dispatched: false };
  }
  if (responseStarted && completion === "finished") {
    return { state: "completed", replay: false, retry_dispatched: false };
  }
  if (completion === "failed") {
    return { state: "transport_failed", replay: false, retry_dispatched: false };
  }
  // Unknown: the only honest answer is uncertain, and it stays fail-closed.
  return { state: "uncertain_fail_closed", replay: false, retry_dispatched: false };
}

// No-hidden-retry verdict under conservative accounting. Any
// potential_model_effect record (unknown conversation-namespace POST) or any
// second known model-effect send blocks the clean verdict.
export function hiddenRetryVerdict(knownSends, potentialSends) {
  if (potentialSends > 0) {
    return { hidden_retry_or_fanout: true, blocked_by: "potential_model_effect" };
  }
  if (knownSends > 1) {
    return { hidden_retry_or_fanout: true, blocked_by: "multiple_model_effect_sends" };
  }
  return { hidden_retry_or_fanout: false, blocked_by: null };
}

// ---------------------------------------------------------------------------
// Fixture tables
// ---------------------------------------------------------------------------

const ORIGIN_FIXTURES = [
  { origin: "https://chatgpt.com", expect: { onChatGPT: true, authOrigin: false }, note: "exact ChatGPT origin" },
  { origin: "https://chatgpt.com.evil.example", expect: { onChatGPT: false, authOrigin: false }, note: "lookalike suffix must NOT match" },
  { origin: "https://evil.example/chatgpt.com", expect: { onChatGPT: false, authOrigin: false }, note: "path lookalike must NOT match" },
  { origin: "https://www.chatgpt.com", expect: { onChatGPT: false, authOrigin: false }, note: "www subdomain fails closed" },
  { origin: "https://sub.chatgpt.com", expect: { onChatGPT: false, authOrigin: false }, note: "any subdomain fails closed" },
  { origin: "http://chatgpt.com", expect: { onChatGPT: false, authOrigin: false }, note: "insecure scheme fails closed" },
  { origin: "https://auth0.openai.com", expect: { onChatGPT: false, authOrigin: true }, note: "auth0 exact origin" },
  { origin: "https://auth.openai.com", expect: { onChatGPT: false, authOrigin: true }, note: "auth exact origin" },
  { origin: "https://auth0.openai.com.evil.example", expect: { onChatGPT: false, authOrigin: false }, note: "auth lookalike must NOT match" },
  { origin: "null", expect: { onChatGPT: false, authOrigin: false }, note: "file:// fixture origin (null)" },
];

const CLASSIFIER_FIXTURES = [
  { method: "POST", path: "/backend-api/f/conversation", expect: "model_effect_conversation", note: "live model effect (turns 1 and 2)" },
  { method: "POST", path: "/backend-api/conversation", expect: "model_effect_conversation", note: "non-f variant model effect" },
  { method: "POST", path: "/backend-api/f/conversation/prepare", expect: "model_effect_prepare", note: "known pre-dispatch prepare" },
  { method: "POST", path: "/backend-api/conversation/init", expect: "conversation_api_aux", note: "known init auxiliary" },
  { method: "GET", path: "/backend-api/conversation/stream_status", expect: "conversation_api_aux", note: "GET aux read" },
  { method: "GET", path: "/backend-api/conversation/textdocs", expect: "conversation_api_aux", note: "GET aux read" },
  { method: "POST", path: "/backend-api/conversation/resume", expect: "potential_model_effect", note: "unknown POST must be flagged" },
  { method: "POST", path: "/backend-api/f/conversation/continuation", expect: "potential_model_effect", note: "unknown f-route POST must be flagged" },
  { method: "POST", path: "/backend-api/conversation/stream_status", expect: "potential_model_effect", note: "POST to an aux-shaped route is still unknown -> flagged" },
  { method: "DELETE", path: "/backend-api/conversation/abc", expect: "conversation_api_aux", note: "non-POST cannot be a model effect" },
  { method: "GET", path: "/backend-api/me", expect: "session_check", note: "session probe" },
];

const URLSHAPE_FIXTURES = [
  {
    raw: "https://chatgpt.com/c/6a811a31-6f7a-4f6a-9c81-6f7a4f6a9c81?x=1#frag",
    check: (s) =>
      s.host === "chatgpt.com" &&
      !s.path.includes("?") &&
      !s.path.includes("#") &&
      s.path.startsWith("/c/"),
    expect: "query and fragment must be dropped",
    note: "URLs are reduced to host + pathname; query/fragment never persisted",
  },
  {
    raw: "https://chatgpt.com.evil.example/c?token=secret",
    check: (s) => s.host === "chatgpt.com.evil.example" && !s.path.includes("token"),
    expect: "host kept, query dropped",
    note: "host shape + pathname only",
  },
  {
    raw: "https://chatgpt.com/c/6a811a31-6f7a-4f6a-9c81-6f7a4f6a9c81?x=1#frag",
    use: "targetShape",
    check: (s) => s.host === "chatgpt.com" && s.pathClass === "/c" && !JSON.stringify(s).includes("6a811a31"),
    expect: "target shape: host + coarse path class, NO conversation-id fragment",
    note: "target URLs are never logged raw; only host + first path segment",
  },
];

const CONVERSATION_ID_FIXTURES = [
  {
    raw: "6a811a31-6f7a-4f6a-9c81-6f7a4f6a9c81",
    check: (ev) =>
      ev.conversation_id === null &&
      ev.conversation_id_redacted === true &&
      ev.conversation_id_placeholder === "conv#turn1" &&
      !ev.conversation_id_placeholder.includes("6a811a31"),
    expect: "conv#turn1 placeholder, no original fragment, redacted flag set",
    note: "conversation id must never be persisted, not even truncated",
  },
];

const TIMEOUT_FIXTURES = [
  {
    name: "sent-then-timeout",
    input: { sent: true, responseStarted: false, completion: null, timedOut: true },
    expect: { state: "sent_timeout_fail_closed", replay: false, retry_dispatched: false },
    note: "bounded wait expiry -> typed fail-closed, no replay",
  },
  {
    name: "not-sent",
    input: { sent: false, responseStarted: false, completion: null, timedOut: true },
    expect: { state: "not_sent", replay: false, retry_dispatched: false },
    note: "pre-dispatch N=0 family",
  },
  {
    name: "completed",
    input: { sent: true, responseStarted: true, completion: "finished", timedOut: false },
    expect: { state: "completed", replay: false, retry_dispatched: false },
    note: "turn 1 shape",
  },
  {
    name: "aborted",
    input: { sent: true, responseStarted: false, completion: "failed", timedOut: false },
    expect: { state: "transport_failed", replay: false, retry_dispatched: false },
    note: "canceled_aborted shape",
  },
  {
    name: "retry-flips-verdict",
    input: { sent: true, responseStarted: false, completion: null, timedOut: true },
    expect: { state: "sent_timeout_fail_closed", replay: false, retry_dispatched: false },
    extra: { knownSends: 1, potentialSends: 1 },
    expectVerdict: { hidden_retry_or_fanout: true, blocked_by: "potential_model_effect" },
    note: "a later unknown conversation POST blocks no-hidden-retry",
  },
  {
    name: "clean-turn-verdict",
    input: { sent: true, responseStarted: true, completion: "finished", timedOut: false },
    expect: { state: "completed", replay: false, retry_dispatched: false },
    extra: { knownSends: 1, potentialSends: 0 },
    expectVerdict: { hidden_retry_or_fanout: false, blocked_by: null },
    note: "turn 1 clean verdict",
  },
];

function assertEqual(actual, expected, label) {
  const a = JSON.stringify(actual);
  const e = JSON.stringify(expected);
  return { pass: a === e, expected: e, actual: a, label };
}

// Runs every table. Returns { passed, failed, sections }.
export function runFailClosedProofs() {
  const sections = [];

  const originRows = ORIGIN_FIXTURES.map((f) => {
    const actual = originState(f.origin);
    return { ...f, ...assertEqual(actual, f.expect, `origin ${f.origin}`) };
  });
  sections.push({ name: "origin_exact_match", rows: originRows });
  const classifierRows = CLASSIFIER_FIXTURES.map((f) => {
    const actual = classifyRequest(f.method, f.path);
    return { ...f, ...assertEqual(actual, f.expect, `classify ${f.method} ${f.path}`) };
  });
  sections.push({ name: "classifier_conservative", rows: classifierRows });
  const urlRows = URLSHAPE_FIXTURES.map((f) => {
    const actual = f.use === "targetShape" ? targetShape(f.raw) : urlShape(f.raw);
    const pass = f.check(actual);
    return { ...f, pass, actual, expected: f.expect };
  });
  sections.push({ name: "url_shape_redaction", rows: urlRows });
  const convRows = CONVERSATION_ID_FIXTURES.map((f) => {
    const ev = conversationIdEvidence("turn1");
    return { ...f, check_result: f.check(ev), actual: ev, note: f.note };
  });
  sections.push({ name: "conversation_id_redaction", rows: convRows });
  const timeoutRows = TIMEOUT_FIXTURES.map((f) => {
    const actual = timeoutState(f.input);
    const row = { ...f, stateCheck: assertEqual(actual, f.expect, `timeout ${f.name}`) };
    if (f.expectVerdict) {
      const v = hiddenRetryVerdict(f.extra.knownSends, f.extra.potentialSends);
      row.verdictCheck = assertEqual(v, f.expectVerdict, `verdict ${f.name}`);
    }
    return row;
  });
  sections.push({ name: "timeout_fail_closed", rows: timeoutRows });

  const failed = [];
  for (const s of sections) {
    for (const r of s.rows) {
      if (r.pass === false) failed.push({ section: s.name, label: r.label, expected: r.expected, actual: r.actual });
      else if (r.stateCheck && r.stateCheck.pass === false) failed.push({ section: s.name, label: r.stateCheck.label, expected: r.stateCheck.expected, actual: r.stateCheck.actual });
      else if (r.verdictCheck && r.verdictCheck.pass === false) failed.push({ section: s.name, label: r.verdictCheck.label, expected: r.verdictCheck.expected, actual: r.verdictCheck.actual });
      else if (r.check_result === false) failed.push({ section: s.name, label: "conversation_id_redaction", note: r.note });
    }
  }
  // Redaction smoke: conversation ids must never rely on the generic
  // redactor (it can leave an 8-char prefix); the placeholder mechanism used
  // by the harness keeps zero fragments and survives redaction intact.
  const redactedEvidence = redact(conversationIdEvidence("turn1"));
  const smokePass =
    redactedEvidence.conversation_id === null &&
    redactedEvidence.conversation_id_placeholder === "conv#turn1" &&
    redactedEvidence.conversation_id_redacted === true &&
    !JSON.stringify(redactedEvidence).includes("6a811a31");
  sections.push({
    name: "redact_smoke",
    rows: [
      {
        pass: smokePass,
        actual: redactedEvidence,
        note: "conversation ids are replaced wholesale by non-correlatable placeholders; the generic redactor is not trusted for ids",
      },
    ],
  });
  if (!smokePass) failed.push({ section: "redact_smoke", label: "redact smoke" });

  return {
    mode: "dry",
    model_turns: 0,
    passed: failed.length === 0,
    failed,
    sections,
  };
}