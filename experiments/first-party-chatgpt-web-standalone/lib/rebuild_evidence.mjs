// rebuild_evidence.mjs - canonical evidence reconstruction for the live run.
//
// The first live run (2026-08-16, budget 2/2 model turns) was executed with
// harness v1, which had accounting-label bugs (classification strings mangled
// by the redactor; turn windows not scoped per turn). The raw sanitized
// records were preserved, then re-derived with the fixed classifier (v2) into
// evidence/network-turns-live.json. Some derived verdicts inside the raw key
// events were NOT corrected by v2 (stale physical_sends/response_started/
// requestId fields), making the evidence package internally inconsistent.
//
// This script (v3) canonicalizes the whole package from the corrected
// transport records + preserved DOM/lifecycle facts:
//   - per-turn accounting (physical_sends, response_started, request ids,
//     in/post-window counts) is RE-DERIVED from the transport records, never
//     copied from the v1 key events;
//   - any conversation id is replaced by a non-correlatable placeholder
//     (no fragment of the real id is kept);
//   - the classifier is the conservative v3 one: an unknown POST in the
//     conversation namespace becomes potential_model_effect and BLOCKS the
//     no-hidden-retry/fan-out verdict;
//   - per-request evidence drops irrelevant "other" traffic (static assets,
//     fonts, avatars, third-party noise) and keeps aggregate counts, so the
//     artifact only exposes shapes needed to prove accounting.
// No new model turns are consumed.
//
// Usage:
//   node lib/rebuild_evidence.mjs            # canonicalize from committed
//                                            # evidence/network-turns-live.json
//                                            # + evidence/live-key-events.json
//   node lib/rebuild_evidence.mjs <log>      # parse a raw v1 run log and
//                                            # write the canonical artifacts

import fs from "node:fs";
import path from "node:path";

import { classifyRequest } from "./network.mjs";
import { conversationIdEvidence, redact, sanitizeConversationPath, urlShape } from "./sanitize.mjs";
import { hiddenRetryVerdict, exhaustiveModelEffectScope, assertNoOrphanedModelEffects } from "./proofs.mjs";

// Conversation ids can appear inside conversation-namespace paths (e.g.
// /backend-api/conversation/<id>/stream_status). They are replaced wholesale
// by a placeholder (while the UUID is intact), then the generic redactor
// runs; this ordering cannot leave a truncated fragment.
function sanitizePath(pathname) {
  return redact(sanitizeConversationPath(String(pathname)));
}

// Evidence never records an absolute filesystem path for the profile. Raw v1
// logs carried the machine-specific checkout path (e.g. ~/Documentos/codigo/
// Runstead/experiments/.../profiles/standalone-spike); the canonical form is
// repo-relative and hermetic.
function normalizeProfilePath(value) {
  if (typeof value !== "string") return value;
  if (/profiles[\\/]standalone-spike$/.test(value)) {
    return "profiles/standalone-spike";
  }
  return value;
}

const HERE = path.dirname(new URL(import.meta.url).pathname);
const EXPERIMENT = path.join(HERE, "..");
const EVIDENCE_DIR = path.join(EXPERIMENT, "evidence");
const OUT_DIR = path.join(EXPERIMENT, "output");

// Harness version history (identified exactly, per review):
//   v1 - executed the live run. Sent-confirmed cancellation semantics
//        (dispatch -> request in flight -> stop clicked before response
//        start). Redactor mangled classification strings; turn windows not
//        per-turn scoped.
//   v2 - fixed classifier + per-turn scoping; dry-validated; evidence rebuilt
//        from the preserved log.
//   v3 - review fixes: exact-origin contract, conservative conversation-
//        namespace classification, sent-confirmed cancellation code aligned
//        to what v1 actually executed, conversation-id placeholders, target
//        URL shaping, deterministic fail-closed/timeout proofs. Dry-validated.
const HARNESS_LIVE_RUN = "v1";
const HARNESS_REBUILD = "v3";
const PROVENANCE = {
  executed_by: `harness ${HARNESS_LIVE_RUN} (live run 2026-08-16, budget 2/2)`,
  canonicalized_by: `lib/rebuild_evidence.mjs (harness ${HARNESS_REBUILD})`,
  note:
    "classification re-derived from method+path with the conservative v3 classifier; " +
    "turns attributed by recorded dispatch timestamps and window/turn fields; " +
    "derived per-turn verdicts rebuilt from transport records, never copied from raw v1 key events; " +
    "conversation ids replaced by non-correlatable placeholders",
};

// Turn boundaries recorded in the v1 live lifecycle (ms epoch).
const DISPATCH_T1 = 1786845745125;
const DISPATCH_T2 = 1786845754010;
const CANCEL_ATTEMPT = 1786845754071;

function parseLog(logPath) {
  const lines = fs.readFileSync(logPath, "utf8").split("\n");
  const events = [];
  for (const line of lines) {
    const m = line.match(/^\[([^\]]+)\] (\{.*\})$/);
    if (!m) continue;
    try {
      const obj = JSON.parse(m[2]);
      obj._kind = m[1];
      events.push(obj);
    } catch {
      // not a JSON line
    }
  }
  return events;
}

// Rebuild per-request records from a raw v1 run log.
function recordsFromLog(events) {
  const requests = new Map();
  for (const e of events) {
    if (!e._kind.startsWith("net:")) continue;
    const kind = e._kind.slice(4);
    if (kind === "request_will_be_sent") {
      const path = sanitizePath(e.path);
      requests.set(e.requestId, {
        requestId: e.requestId,
        method: e.method,
        host: e.host,
        path,
        classification: classifyRequest(e.method, path),
        window: e.window,
        turn: e.turn ?? 0,
        status: null,
        mimeType: null,
        completion: null,
        errorText: null,
        canceled: null,
        streamBytes: 0,
        firstSeenTs: e.ts,
      });
    } else if (kind === "response_received") {
      const r = requests.get(e.requestId);
      if (r) {
        r.status = e.status;
        r.mimeType = e.mimeType;
      }
    } else if (kind === "loading_finished") {
      const r = requests.get(e.requestId);
      if (r) {
        r.completion = "finished";
        r.streamBytes = e.streamBytes ?? 0;
      }
    } else if (kind === "loading_failed") {
      const r = requests.get(e.requestId);
      if (r) {
        r.completion = "failed";
        r.errorText = e.errorText;
        r.canceled = e.canceled;
      }
    }
  }
  return [...requests.values()].sort((a, b) => a.firstSeenTs - b.firstSeenTs);
}

// Load the canonical transport records + raw key events currently committed.
function loadArtifacts() {
  const net = JSON.parse(
    fs.readFileSync(path.join(EVIDENCE_DIR, "network-turns-live.json"), "utf8")
  );
  const ke = JSON.parse(
    fs.readFileSync(path.join(EVIDENCE_DIR, "live-key-events.json"), "utf8")
  );
  return {
    requests: (net.requests ?? []).map((r) => {
      const path = sanitizePath(r.path);
      return { ...r, path, classification: classifyRequest(r.method, path) };
    }),
    events: ke.events ?? [],
    storedAggregates: net.aggregates ?? null,
  };
}

function turnOf(ts) {
  if (ts >= DISPATCH_T2) return 2;
  if (ts >= DISPATCH_T1) return 1;
  return 0;
}

// Per-turn derived accounting from transport records (never from raw events).
// Uses the SAME exhaustive per-turn scope rule as the runtime
// (exhaustiveModelEffectScope / hasOrphanedModelEffects), so runtime and
// rebuild agree and no between-turns (pre-turn) window can be orphaned.
function turnAccounting(requests, turn) {
  const inTurn = requests.filter((r) => r.turn === turn && r.window === "turn");
  const post = requests.filter((r) => r.turn === turn && r.window === "post_turn");
  const countBy = (reqs) => {
    const counts = {};
    for (const r of reqs) {
      counts[r.classification] = (counts[r.classification] || 0) + 1;
    }
    return counts;
  };
  const scope = exhaustiveModelEffectScope(requests, turn);
  const known = scope.known;
  const potential = scope.potential;
  const conversations = inTurn.filter((r) => r.classification === "model_effect_conversation");

  const verdict = hiddenRetryVerdict(known.length, potential.length);

  return {
    physical_sends: known.length,
    prepare_sends: inTurn.filter((r) => r.classification === "model_effect_prepare").length,
    potential_model_effect: potential.length,
    response_started: conversations.some((r) => r.completion === "finished" || r.streamBytes > 0),
    in_window_counts: countBy(inTurn),
    post_window_counts: countBy(post),
    conversations: known.map((r) => ({
      requestId: r.requestId,
      status: r.status,
      mimeType: r.mimeType,
      completion: r.completion,
      errorText: r.errorText,
      canceled: r.canceled,
      streamBytes: r.streamBytes,
    })),
    hidden_retry_or_fanout: verdict.hidden_retry_or_fanout,
    verdict_blocked_by: verdict.blocked_by,
  };
}

function canonicalKeyEvents(events, requests) {
  const out = [];
  for (const e of events) {
    const k = e._kind;
    if (
      k === "turn1" ||
      k === "turn2" ||
      k === "response_started" ||
      k === "sent_confirmed" ||
      k === "cancel_attempt" ||
      k === "baseline_done"
    ) {
      continue; // rebuilt below from transport records + preserved facts
    }
    // Preserve raw DOM/lifecycle facts; no conversation id fragments and no
    // absolute profile path.
    const copy = { ...e, _kind: undefined };
    if (copy.profilePath) copy.profilePath = normalizeProfilePath(copy.profilePath);
    if (copy.profile && typeof copy.profile === "object" && copy.profile.path) {
      copy.profile.path = normalizeProfilePath(copy.profile.path);
    }
    if (copy.conversation_id) {
      const { conversation_id, ...rest } = copy;
      Object.assign(copy, conversationIdEvidence(k === "turn1" ? "turn1" : "turn2"));
      copy.conversation_id_raw_fragment_removed = true;
    }
    out.push({ ...copy, _kind: k });
  }

  const t1raw = events.find((e) => e._kind === "turn1");
  const t2raw = events.find((e) => e._kind === "turn2");
  const cancelRaw = events.find((e) => e._kind === "cancel_attempt");
  const a1 = turnAccounting(requests, 1);
  const a2 = turnAccounting(requests, 2);

  // Rebuild baseline_done from transport records (v1 raw counts used the
  // mangled classifier and can disagree with the canonical aggregate).
  const baselineReqs = requests.filter((r) => r.window === "baseline" && r.turn === 0);
  const baselineCounts = {};
  for (const r of baselineReqs) {
    baselineCounts[r.classification] = (baselineCounts[r.classification] || 0) + 1;
  }
  out.push({
    kind: "baseline_done",
    counts: baselineCounts,
    modelEffectInBaseline: baselineCounts.model_effect_conversation ?? 0,
    _kind: "baseline_done",
  });

  // Canonical transport-level key events (one per turn, correct request ids).
  for (const [turn, acct, label] of [
    [1, a1, "turn1"],
    [2, a2, "turn2"],
  ]) {
    const conv = acct.conversations[0] ?? null;
    out.push({
      kind: "sent_confirmed",
      turn: turn,
      requestId: conv ? conv.requestId : null,
      classification: "model_effect_conversation",
      _kind: "sent_confirmed",
    });
    if (acct.response_started && conv) {
      out.push({
        kind: "response_started",
        turn: turn,
        requestId: conv.requestId,
        classification: "model_effect_conversation",
        status: conv.status,
        mimeType: conv.mimeType,
        _kind: "response_started",
      });
    }
  }

  const t1 = {
    kind: "turn1",
    outcome: t1raw?.outcome ?? "ok",
    physical_sends: a1.physical_sends,
    prepare_sends: a1.prepare_sends,
    potential_model_effect: a1.potential_model_effect,
    in_window_counts: a1.in_window_counts,
    post_window_counts: a1.post_window_counts,
    response_started: a1.response_started,
    loading_completions: a1.conversations.map((c) => ({
      requestId: c.requestId,
      completion: c.completion,
      errorText: c.errorText,
      status: c.status,
      mimeType: c.mimeType,
      streamBytes: c.streamBytes,
    })),
    dom_terminal: t1raw?.dom_terminal ?? null,
    dom_busy_false: t1raw?.dom_busy_false ?? null,
    token_present: t1raw?.token_present ?? null,
    text_length: t1raw?.text_length ?? null,
    text_hash: t1raw?.text_hash ?? null,
    ...conversationIdEvidence("turn1"),
    conversation_id_raw_fragment_removed: true,
    hidden_retry_or_fanout: a1.hidden_retry_or_fanout,
    verdict_blocked_by: a1.verdict_blocked_by,
    _kind: "turn1",
  };
  out.push(t1);

  const t2 = {
    kind: "turn2",
    outcome:
      a1.conversations[0] && a2.conversations[0]?.completion === "failed" &&
      /ERR_ABORTED/.test(a2.conversations[0]?.errorText || "")
        ? "canceled_aborted"
        : "uncertain",
    physical_sends: a2.physical_sends,
    prepare_sends: a2.prepare_sends,
    potential_model_effect: a2.potential_model_effect,
    in_window_counts: a2.in_window_counts,
    post_window_counts: a2.post_window_counts,
    response_started: a2.response_started,
    loading: a2.conversations[0]
      ? {
          completion: a2.conversations[0].completion,
          errorText: a2.conversations[0].errorText,
          canceled: a2.conversations[0].canceled,
          streamBytes: a2.conversations[0].streamBytes,
        }
      : null,
    dom_after_cancel: t2raw?.dom_after_cancel ?? null,
    hidden_retry_or_fanout: a2.hidden_retry_or_fanout,
    verdict_blocked_by: a2.verdict_blocked_by,
    _kind: "turn2",
  };
  out.push(t2);

  const conv2 = a2.conversations[0] ?? null;
  out.push({
    kind: "cancel_attempt",
    clicked: cancelRaw?.clicked ?? true,
    reason: cancelRaw?.reason ?? null,
    requestId: conv2 ? conv2.requestId : null,
    ms_after_dispatch: CANCEL_ATTEMPT - DISPATCH_T2,
    _kind: "cancel_attempt",
  });

  // Keep event order sensible: sent/response events just before their turn.
  return out.sort((a, b) => {
    const order = {
      spike_start: 0,
      self_audit: 1,
      browser_launched: 2,
      cdp_connected: 3,
      session_state: 4,
      harness_installed: 5,
      baseline_done: 6,
      composer_verified: 7,
      dispatch_clicked: 8,
      sent_confirmed: 9,
      response_started: 10,
      turn1: 11,
      turn2: 12,
      cancel_attempt: 13,
      crash_test: 14,
      crash_classified: 15,
      done: 16,
    };
    const ao = order[a._kind] ?? 20;
    const bo = order[b._kind] ?? 20;
    return ao - bo;
  });
}

function aggregate(requests) {
  const byClass = {};
  const byWindowTurn = {};
  for (const r of requests) {
    byClass[r.classification] = (byClass[r.classification] || 0) + 1;
    const key = `${r.window}/${r.turn}`;
    byWindowTurn[key] = byWindowTurn[key] || {};
    byWindowTurn[key][r.classification] = (byWindowTurn[key][r.classification] || 0) + 1;
  }
  return { counts_by_class: byClass, counts_by_window_turn: byWindowTurn };
}

// The per-request artifact drops "other" traffic, so on later rebuilds the
// full-set totals (including "other") must be carried from the previously
// stored aggregates instead of recomputed from the filtered list. Kept
// classes are always recomputed from the actual records.
function mergeAggregates(computed, stored) {
  if (!stored) return computed;
  const merged = { counts_by_class: { ...(stored.counts_by_class ?? {}) }, counts_by_window_turn: {} };
  for (const [k, v] of Object.entries(computed.counts_by_class)) {
    merged.counts_by_class[k] = v;
  }
  const windows = new Set([
    ...Object.keys(stored.counts_by_window_turn ?? {}),
    ...Object.keys(computed.counts_by_window_turn),
  ]);
  for (const win of windows) {
    const bucket = { ...((stored.counts_by_window_turn ?? {})[win] ?? {}) };
    for (const [k, v] of Object.entries(computed.counts_by_window_turn[win] ?? {})) {
      bucket[k] = v;
    }
    merged.counts_by_window_turn[win] = bucket;
  }
  return merged;
}

function build(requests, events, storedAggregates) {
  const keyEvents = canonicalKeyEvents(events, requests);
  const agg = mergeAggregates(aggregate(requests), storedAggregates);
  const t1 = keyEvents.find((e) => e._kind === "turn1");
  const t2 = keyEvents.find((e) => e._kind === "turn2");
  const cancel = keyEvents.find((e) => e._kind === "cancel_attempt");
  const crash = keyEvents.find((e) => e._kind === "crash_classified");

  // Per-request evidence: keep only shapes needed to prove accounting.
  // "other" traffic (static assets, fonts, avatars, third-party noise) is
  // dropped from the per-request list; its aggregate counts are preserved.
  const kept = requests.filter((r) => r.classification !== "other");
  const droppedOther = agg.counts_by_class.other ?? 0;

  const networkEvidence = {
    provenance: PROVENANCE,
    redaction: {
      per_request_kept: "accounting-relevant classes only (model_effect*, potential_model_effect, conversation*, session_check, sentinel_aux, ces_telemetry, backend_api_aux, static_asset)",
      per_request_removed: {
        classification: "other",
        count: droppedOther,
        why: "static assets, fonts, avatars, third-party/noise traffic: irrelevant to send accounting and exposes personal/identifying path shapes",
      },
      query_fragments: "always dropped (urlShape)",
      conversation_ids: "never persisted; non-correlatable placeholder only",
      target_urls: "never logged raw; host+pathname shape only",
    },
    requests: kept,
    aggregates: agg,
  };

  const summary = {
    mode: "live",
    date: "2026-08-16",
    harness_version: { executed_by: HARNESS_LIVE_RUN, canonicalized_by: HARNESS_REBUILD },
    provenance: PROVENANCE,
    turn1: {
      outcome: t1?.outcome ?? "unknown",
      physical_sends: t1?.physical_sends ?? 0,
      prepare_sends: t1?.prepare_sends ?? 0,
      potential_model_effect: t1?.potential_model_effect ?? 0,
      response_started: t1?.response_started ?? false,
      completion: t1?.loading_completions?.[0]?.completion ?? null,
      status: t1?.loading_completions?.[0]?.status ?? null,
      mimeType: t1?.loading_completions?.[0]?.mimeType ?? null,
      streamBytes: t1?.loading_completions?.[0]?.streamBytes ?? 0,
      token_present: t1?.token_present ?? null,
      text_length: t1?.text_length ?? null,
      dom_terminal: t1?.dom_terminal ?? null,
      ...conversationIdEvidence("turn1"),
      conversation_id_raw_fragment_removed: true,
      hidden_retry_or_fanout: t1?.hidden_retry_or_fanout ?? null,
      correlated: true,
    },
    turn2: {
      outcome: t2?.outcome ?? "uncertain",
      physical_sends: t2?.physical_sends ?? 0,
      prepare_sends: t2?.prepare_sends ?? 0,
      potential_model_effect: t2?.potential_model_effect ?? 0,
      response_started: t2?.response_started ?? false,
      completion: t2?.loading?.completion ?? null,
      errorText: t2?.loading?.errorText ?? null,
      canceled: t2?.loading?.canceled ?? null,
      cancel_clicked: cancel?.clicked ?? null,
      cancel_ms_after_dispatch: cancel?.ms_after_dispatch ?? null,
      hidden_retry_or_fanout: t2?.hidden_retry_or_fanout ?? null,
    },
    crash,
  };

  const keyEventsEvidence = {
    provenance: PROVENANCE,
    note:
      "canonical key events: per-turn derived verdicts rebuilt from transport records; " +
      "raw v1 stale fields (physical_sends/response_started/requestId) discarded; " +
      "conversation ids replaced by non-correlatable placeholders",
    events: keyEvents,
  };

  return { networkEvidence, keyEventsEvidence, summary, t1, t2, agg };
}

function writeArtifacts(networkEvidence, keyEventsEvidence, summary) {
  fs.mkdirSync(EVIDENCE_DIR, { recursive: true });
  fs.mkdirSync(OUT_DIR, { recursive: true });
  fs.writeFileSync(path.join(EVIDENCE_DIR, "network-turns-live.json"), JSON.stringify(networkEvidence, null, 2));
  fs.writeFileSync(path.join(EVIDENCE_DIR, "live-key-events.json"), JSON.stringify(keyEventsEvidence, null, 2));
  fs.writeFileSync(path.join(OUT_DIR, "summary-live.json"), JSON.stringify(summary, null, 2));
}

function main() {
  const logPath = process.argv[2];
  let requests;
  let events;
  let storedAggregates = null;
  if (logPath && fs.existsSync(logPath)) {
    const parsed = parseLog(logPath);
    requests = recordsFromLog(parsed);
    events = parsed;
  } else {
    const art = loadArtifacts();
    requests = art.requests;
    events = art.events;
    storedAggregates = art.storedAggregates;
  }

  const { networkEvidence, keyEventsEvidence, summary, t1, t2, agg } = build(requests, events, storedAggregates);
  writeArtifacts(networkEvidence, keyEventsEvidence, summary);

  console.log("wrote evidence/network-turns-live.json, evidence/live-key-events.json, output/summary-live.json");
  console.log("turn1 sends:", summary.turn1.physical_sends, "| response_started:", summary.turn1.response_started, "| potential:", summary.turn1.potential_model_effect);
  console.log("turn2 sends:", summary.turn2.physical_sends, "| outcome:", summary.turn2.outcome, "| response_started:", summary.turn2.response_started, "| potential:", summary.turn2.potential_model_effect);
  console.log("per-request records kept:", networkEvidence.requests.length, "| removed other:", networkEvidence.redaction.per_request_removed.count);
  console.log("all windows:", JSON.stringify(agg.counts_by_window_turn));

  // Internal consistency assertion: artifacts must agree with each other.
  const turn1Sends = t1.physical_sends;
  if (turn1Sends !== 1 || summary.turn1.physical_sends !== 1) {
    throw new Error("consistency check failed: turn1 physical_sends must be 1");
  }
  if (t2.outcome === "canceled_aborted" && t2.loading?.errorText !== "net::ERR_ABORTED") {
    throw new Error("consistency check failed: canceled_aborted requires ERR_ABORTED");
  }
  if (summary.turn2.outcome === "canceled_aborted" && summary.turn2.response_started) {
    throw new Error("consistency check failed: canceled_aborted cannot have response_started");
  }
  // Exhaustive accounting gate: no between-turns (or other) model-effect
  // request may escape every turn's verdict scope. The canonical evidence
  // must not contain an orphaned window. Same authority as the runtime: an
  // orphaned request aborts the rebuild with a non-success exit.
  assertNoOrphanedModelEffects(requests, "rebuild");
}

main();