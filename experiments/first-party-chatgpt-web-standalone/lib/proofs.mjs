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

// EXHAUSTIVE per-turn model-effect scope. Shared by the runtime fanout logic
// (run_spike.mjs) and the canonical rebuild (rebuild_evidence.mjs) so both
// apply the SAME rule and no between-turns window can be orphaned.
//
// A turn N's verdict scope is every request in:
//   - pre_turn[N]  (explicit between-turns window attributed to turn N)
//   - turn[N]
//   - post_turn[N]
//   - for N == 1 only, the initial baseline (turn 0, window "baseline"),
//     because the pre-any-turn window belongs to the first turn's verdict.
//
// Every request between the session's initial baseline and the final
// post-turn close therefore belongs to exactly one turn's scope (enforced by
// hasOrphanedModelEffects/assertNoOrphanedModelEffects over the whole run).
export function exhaustiveModelEffectScope(requests, turnNumber) {
  const inScope = (r) =>
    (r.turn === turnNumber &&
      (r.window === "turn" || r.window === "post_turn")) ||
    (r.window === "pre_turn" && r.turn === turnNumber) ||
    (turnNumber === 1 && r.turn === 0 && r.window === "baseline");

  const known = [];
  const potential = [];
  for (const r of requests) {
    const c = r.classification;
    if (c !== "model_effect_conversation" && c !== "potential_model_effect") continue;
    if (!inScope(r)) continue;
    if (c === "model_effect_conversation") known.push(r);
    else potential.push(r);
  }
  return { known, potential };
}

// A model-effect request is attributable iff it falls in some turn's scope.
function isAnyTurnScope(r) {
  if (r.turn === 0 && r.window === "baseline") return true; // attributed to turn 1
  if (r.window === "turn" || r.window === "post_turn" || r.window === "pre_turn") {
    return r.turn > 0;
  }
  return false;
}

// True if any model-effect request is NOT in any turn's exhaustive scope
// (i.e. it would be orphaned / escape all verdicts). Callers must fail closed
// when this is true: "no orphaned window is acceptable".
export function hasOrphanedModelEffects(requests) {
  for (const r of requests) {
    const c = r.classification;
    if (c !== "model_effect_conversation" && c !== "potential_model_effect") continue;
    if (!isAnyTurnScope(r)) return true;
  }
  return false;
}

// Central fail-closed assertion for orphaned model-effect requests. Shared by
// the runtime (run_spike.mjs) and the canonical rebuild (rebuild_evidence.mjs)
// so both terminate identically when any model-effect request escapes every
// turn's exhaustive scope. Throwing here forces a non-zero exit / non-success
// decision path BEFORE a clean/final verdict is emitted, so detection can
// never be observed while the run still concludes success. `context` labels the
// failing turn for diagnostics.
export function assertNoOrphanedModelEffects(requests, context) {
  if (hasOrphanedModelEffects(requests)) {
    throw new Error(
      `orphaned model-effect request outside every turn scope (${context ?? "unknown"})`
    );
  }
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

// A request is a helper for the exhaustive-scope fixtures.
const FIXT = (turn, window, classification) => ({ turn, window, classification });

// Exhaustive accounting: no between-turns (pre-turn) model effect may escape a
// verdict. These fixtures FAIL under the old behavior (which left the
// inter-turn baseline window orphaned) and pass with exhaustiveModelEffectScope
// + hasOrphanedModelEffects.
const EXHAUSTIVE_SCOPE_FIXTURES = [
  {
    name: "clean-two-turn-run",
    requests: [
      FIXT(0, "baseline", "backend_api_aux"),
      FIXT(1, "turn", "model_effect_conversation"),
      FIXT(1, "post_turn", "sentinel_aux"),
      FIXT(2, "turn", "model_effect_conversation"),
      FIXT(2, "post_turn", "sentinel_aux"),
    ],
    expect: {
      turn1: { known: 1, potential: 0 },
      turn2: { known: 1, potential: 0 },
      orphaned: false,
    },
    note: "no model effect in pre-turn windows; clean",
  },
  {
    name: "pre-turn-known-blocks-turn2",
    requests: [
      FIXT(0, "baseline", "backend_api_aux"),
      FIXT(1, "turn", "model_effect_conversation"),
      FIXT(1, "post_turn", "sentinel_aux"),
      FIXT(2, "pre_turn", "model_effect_conversation"), // orphaned under OLD rule
      FIXT(2, "turn", "model_effect_conversation"),
    ],
    expect: {
      turn1: { known: 1, potential: 0 },
      turn2: { known: 2, potential: 0 }, // pre-turn known + in-turn known
      orphaned: false, // attributed to turn 2, NOT orphaned
    },
    note: "a known model effect in turn 2's pre-turn window belongs to turn 2's scope",
  },
  {
    name: "pre-turn-potential-blocks-and-not-orphaned",
    requests: [
      FIXT(0, "baseline", "backend_api_aux"),
      FIXT(1, "turn", "model_effect_conversation"),
      FIXT(1, "post_turn", "sentinel_aux"),
      FIXT(2, "pre_turn", "potential_model_effect"),
      FIXT(2, "turn", "model_effect_conversation"),
    ],
    expect: {
      turn1: { known: 1, potential: 0 },
      turn2: { known: 1, potential: 1 },
      orphaned: false,
    },
    note: "a potential model effect in turn 2's pre-turn window blocks the clean verdict for turn 2 and is not orphaned",
  },
  {
    name: "orphaned-window-detected",
    requests: [
      FIXT(0, "baseline", "backend_api_aux"),
      FIXT(1, "turn", "model_effect_conversation"),
      FIXT(0, "turn", "potential_model_effect"), // wrong turn attribution -> orphaned
    ],
    expect: { turn1: { known: 1, potential: 0 }, turn2: { known: 0, potential: 0 }, orphaned: true },
    note: "a model effect that no turn's scope covers (turn 0 / window turn) is detected as orphaned",
  },
];

// DECISION-PATH proof (review #4954384690): orphaned model-effect detection
// must TERMINATE the run (throw -> non-zero/non-success) and not merely be
// observed. The live harness relies on this to fail closed before a clean /
// final verdict is emitted. These fixtures drive the same shared authority the
// runtime uses (assertNoOrphanedModelEffects) for both turns and the rebuild.
const ORPHANED_TERMINAL_FIXTURES = [
  {
    name: "clean-run-success",
    requests: [
      FIXT(0, "baseline", "backend_api_aux"),
      FIXT(1, "turn", "model_effect_conversation"),
      FIXT(1, "post_turn", "sentinel_aux"),
      FIXT(2, "pre_turn", "sentinel_aux"),
      FIXT(2, "turn", "model_effect_conversation"),
      FIXT(2, "post_turn", "sentinel_aux"),
    ],
    terminal: false,
    note: "clean run: assertNoOrphanedModelEffects returns (non-terminal / success path)",
  },
  {
    name: "orphaned-after-turn2-terminates",
    requests: [
      FIXT(0, "baseline", "backend_api_aux"),
      FIXT(1, "turn", "model_effect_conversation"),
      FIXT(2, "pre_turn", "sentinel_aux"),
      FIXT(2, "turn", "model_effect_conversation"),
      FIXT(0, "turn", "potential_model_effect"), // orphan present at turn-2 accounting time
    ],
    terminal: true,
    note: "an orphaned request present when turn 2 accounting runs TERMINATES via assertNoOrphanedModelEffects (non-success), mirroring the turn-2 fail-closed path that previously only logged",
  },
  {
    name: "orphaned-after-turn1-terminates",
    requests: [
      FIXT(0, "baseline", "backend_api_aux"),
      FIXT(1, "turn", "model_effect_conversation"),
      FIXT(0, "turn", "potential_model_effect"), // orphan detected at turn-1 accounting
    ],
    terminal: true,
    note: "an orphaned request at turn-1 accounting TERMINATES (non-success), mirroring the turn-1 fail-closed path",
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

  // Exhaustive per-turn accounting: no between-turns (pre-turn) model effect
  // may escape a verdict, and orphaned windows are detected and reported.
  const exhaustiveRows = EXHAUSTIVE_SCOPE_FIXTURES.map((f) => {
    const scope1 = exhaustiveModelEffectScope(f.requests, 1);
    const scope2 = exhaustiveModelEffectScope(f.requests, 2);
    const orphaned = hasOrphanedModelEffects(f.requests);
    const actual = {
      turn1: { known: scope1.known.length, potential: scope1.potential.length },
      turn2: { known: scope2.known.length, potential: scope2.potential.length },
      orphaned,
    };
    return {
      ...f,
      scopeCheck: assertEqual(actual, f.expect, `exhaustive ${f.name}`),
      actual,
    };
  });
  sections.push({ name: "exhaustive_model_effect_scope", rows: exhaustiveRows });

  // DECISION-PATH proof: orphaned model-effect detection must TERMINATE the run
  // (throw -> non-zero/non-success), never merely be observed while a clean
  // verdict is still emitted. Drives the same shared authority the runtime and
  // rebuild call (assertNoOrphanedModelEffects).
  const orphanedTerminalRows = ORPHANED_TERMINAL_FIXTURES.map((f) => {
    let threw = false;
    try {
      assertNoOrphanedModelEffects(f.requests, f.name);
    } catch {
      threw = true;
    }
    const passed = threw === f.terminal;
    return {
      ...f,
      pass: passed,
      threw,
      terminalCheck: assertEqual(
        { terminal: threw },
        { terminal: f.terminal },
        `orphaned-terminal ${f.name}`
      ),
    };
  });
  sections.push({ name: "orphaned_effect_fail_closed_terminal", rows: orphanedTerminalRows });

  const failed = [];
  for (const s of sections) {
    for (const r of s.rows) {
      if (r.pass === false) failed.push({ section: s.name, label: r.label, expected: r.expected, actual: r.actual });
      else if (r.stateCheck && r.stateCheck.pass === false) failed.push({ section: s.name, label: r.stateCheck.label, expected: r.stateCheck.expected, actual: r.stateCheck.actual });
      else if (r.verdictCheck && r.verdictCheck.pass === false) failed.push({ section: s.name, label: r.verdictCheck.label, expected: r.verdictCheck.expected, actual: r.verdictCheck.actual });
      else if (r.scopeCheck && r.scopeCheck.pass === false) failed.push({ section: s.name, label: r.scopeCheck.label, expected: r.scopeCheck.expected, actual: r.scopeCheck.actual });
      else if (r.terminalCheck && r.terminalCheck.pass === false) failed.push({ section: s.name, label: r.terminalCheck.label, expected: r.terminalCheck.expected, actual: r.terminalCheck.actual });
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