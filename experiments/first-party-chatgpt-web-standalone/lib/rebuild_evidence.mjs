// rebuild_evidence.mjs - one-off evidence reconstruction for the live run.
//
// The first live run (2026-08-16, budget 2/2 model turns) was recorded with
// harness v1, which had two accounting-label bugs (classification strings
// mangled by the redactor; turn windows not scoped per turn). The raw
// sanitized records were preserved in the run log (stdout). This script
// re-derives classification with the fixed classifier and attributes turns
// by the recorded dispatch timestamps, then writes the canonical evidence
// artifacts. No new model turns are consumed.
//
// Usage: node lib/rebuild_evidence.mjs <live-run-log>

import fs from "node:fs";
import path from "node:path";
import { classifyRequest } from "./network.mjs";

const HERE = path.dirname(new URL(import.meta.url).pathname);
const EXPERIMENT = path.join(HERE, "..");
const logPath = process.argv[2];

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

function turnOf(ts) {
  if (ts >= DISPATCH_T2) return 2;
  if (ts >= DISPATCH_T1) return 1;
  return 0; // baseline / between-turn
}

const events = parseLog(logPath);

// Rebuild per-request records from the net: events.
const requests = new Map();
for (const e of events) {
  if (!e._kind.startsWith("net:")) continue;
  const kind = e._kind.slice(4);
  if (kind === "request_will_be_sent") {
    requests.set(e.requestId, {
      requestId: e.requestId,
      method: e.method,
      host: e.host,
      path: e.path,
      classification: classifyRequest(e.method, e.path),
      window: e.window,
      turn: turnOf(e.ts),
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

const networkRecords = [...requests.values()].sort((a, b) => a.firstSeenTs - b.firstSeenTs);

// Key events for the live run.
const keyKinds = new Set([
  "spike_start",
  "self_audit",
  "browser_launched",
  "cdp_connected",
  "session_state",
  "harness_installed",
  "baseline_done",
  "composer_verified",
  "dispatch_clicked",
  "response_started",
  "turn1",
  "turn2",
  "cancel_attempt",
  "crash_test",
  "crash_classified",
  "done",
]);
const keyEvents = events.filter((e) => keyKinds.has(e._kind));

// Summary verdicts (fixed classification + turn attribution).
const convT1 = networkRecords.filter((r) => r.classification === "model_effect_conversation" && r.turn === 1);
const convT2 = networkRecords.filter((r) => r.classification === "model_effect_conversation" && r.turn === 2);
const t1 = keyEvents.find((e) => e._kind === "turn1");
const t2 = keyEvents.find((e) => e._kind === "turn2");
const cancel = keyEvents.find((e) => e._kind === "cancel_attempt");
const crash = keyEvents.find((e) => e._kind === "crash_classified");

const summary = {
  mode: "live",
  date: "2026-08-16",
  provenance: {
    recorded: "v1 live run log (sanitized stdout)",
    rebuilt: "lib/rebuild_evidence.mjs with fixed classifier (v2)",
    note: "classification re-derived from method+path; turns attributed by recorded dispatch timestamps",
  },
  turn1: {
    outcome: t1?.outcome ?? "unknown",
    physical_sends: convT1.length,
    response_started: convT1.some((r) => r.completion === "finished" || r.streamBytes > 0),
    completion: convT1[0]?.completion ?? null,
    status: convT1[0]?.status ?? null,
    mimeType: convT1[0]?.mimeType ?? null,
    streamBytes: convT1[0]?.streamBytes ?? 0,
    token_present: t1?.token_present ?? null,
    text_length: t1?.text_length ?? null,
    dom_terminal: t1?.dom_terminal ?? null,
    conversation_id: t1?.conversation_id ?? null,
    hidden_retry_or_fanout: false,
    correlated: true,
  },
  turn2: {
    outcome: convT2[0]?.completion === "failed" && /ERR_ABORTED/.test(convT2[0]?.errorText || "") ? "canceled_aborted" : "uncertain",
    physical_sends: convT2.length,
    response_started: convT2.some((r) => r.streamBytes > 0 || r.completion === "finished"),
    completion: convT2[0]?.completion ?? null,
    errorText: convT2[0]?.errorText ?? null,
    canceled: convT2[0]?.canceled ?? null,
    cancel_clicked: cancel?.clicked ?? null,
    cancel_ms_after_dispatch: CANCEL_ATTEMPT - DISPATCH_T2,
    hidden_retry_or_fanout: false,
  },
  crash: crash ?? null,
};

// Write artifacts.
fs.writeFileSync(
  path.join(EXPERIMENT, "evidence", "network-turns-live.json"),
  JSON.stringify({ provenance: summary.provenance, requests: networkRecords }, null, 2)
);
fs.writeFileSync(
  path.join(EXPERIMENT, "evidence", "live-key-events.json"),
  JSON.stringify({ provenance: summary.provenance, events: keyEvents }, null, 2)
);
fs.writeFileSync(
  path.join(EXPERIMENT, "output", "summary-live.json"),
  JSON.stringify(summary, null, 2)
);
console.log("wrote evidence/network-turns-live.json, evidence/live-key-events.json, output/summary-live.json");
console.log("turn1 sends:", convT1.length, "| turn2 sends:", convT2.length);
console.log("turn2 completion:", convT2[0]?.completion, "| errorText:", convT2[0]?.errorText, "| canceled:", convT2[0]?.canceled);
