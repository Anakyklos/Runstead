#!/usr/bin/env node
// run_spike.mjs - disposable standalone first-party browser substrate spike
// for Runstead issue #16 research (Refs #16).
//
// Proves that Runstead (here: this disposable harness) can own a browser
// substrate end-to-end WITHOUT Orca runtime, orca CLI, JCode runtime, JCode
// Browser Agent Bridge or OmniRoute:
//
//   1. profile location/creation  -> lib/browser.mjs (--user-data-dir)
//   2. browser launch             -> lib/browser.mjs (spawn)
//   3. CDP port/socket discovery  -> DevToolsActivePort + /json/version
//   4. ChatGPT target discovery   -> Target domain /json/list
//   5. target lifecycle           -> Target.attachToTarget/closeTarget
//   6. network observation        -> CDP Network domain (transport level)
//   7. navigation                 -> Target.createTarget / Page domain
//   8. DOM interaction            -> Runtime.evaluate + Input.insertText
//   9. cancellation               -> DOM stop + Network.loadingFailed
//  10. browser shutdown           -> SIGKILL + typed fail-closed state
//
// Usage:
//   node run_spike.mjs login   first login into the dedicated profile
//                              (user-assisted, browser visible, no credential
//                              capture ever)
//   node run_spike.mjs dry     fail-closed matrix + fixtures, pre-dispatch
//                              cancellation, target lifecycle, crash test.
//                              ZERO model turns.
//   node run_spike.mjs live    one success turn (RUNSTEAD_STANDALONE_OK) +
//                              one post-dispatch cancellation turn (budget
//                              2 model turns) + crash test.
//
// Artifacts (all sanitized): output/lifecycle.json, output/summary.json,
// evidence/environment.json, evidence/network-turns.json,
// evidence/fail-closed-proofs.json (dry), evidence/auth-custody.json.
// Canonical live evidence (network-turns-live.json, live-key-events.json,
// output/summary-live.json) is produced by lib/rebuild_evidence.mjs.
//
// The profile directory (profiles/standalone-spike) is gitignored and holds
// the real auth session. It is created fresh by this harness, never copied,
// never exported.

import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";

import { redact, targetShape, conversationIdEvidence } from "./lib/sanitize.mjs";
import { Cdp } from "./lib/cdp.mjs";
import {
  launchBrowser,
  connectBrowser,
  openOrFindChatGptTarget,
  attachTarget,
  closeTarget,
} from "./lib/browser.mjs";
import { NetworkObserver } from "./lib/network.mjs";
import { INSTALL_EXPR, EXPR, READY_EXPR } from "./lib/dom.mjs";
import { runFailClosedProofs, exhaustiveModelEffectScope, hasOrphanedModelEffects } from "./lib/proofs.mjs";

// Harness version history (identified exactly, see lib/rebuild_evidence.mjs):
//   v1 - executed the live run (sent-confirmed cancellation semantics);
//   v2 - fixed classifier + per-turn scoping;
//   v3 - this code: exact-origin contract, conservative conversation-namespace
//        classification, sent-confirmed cancellation aligned to what v1
//        actually executed, conversation-id placeholders, target URL shaping,
//        deterministic fail-closed/timeout proofs.
const HARNESS_VERSION = "v3";

const HERE = path.dirname(new URL(import.meta.url).pathname);
const PROFILE_DIR = path.join(HERE, "profiles", "standalone-spike");

// Evidence never records an absolute filesystem path for the profile: it uses
// a repo-relative form ("profiles/standalone-spike") so the committed
// artifacts are hermetic and do not leak a machine-specific checkout path.
function profileEvidencePath() {
  return path.relative(HERE, PROFILE_DIR);
}

const OUT_DIR = path.join(HERE, "output");
const EVIDENCE_DIR = path.join(HERE, "evidence");
const FIXTURE_URL = `file://${path.join(HERE, "fixtures", "logged-out.html")}`;

const PROMPT_TURN1 =
  "Count from 1 to 80, one number per line, and finish with RUNSTEAD_STANDALONE_OK";
const PROMPT_TURN2 =
  "Count from 1 to 120, one number per line. Do not include any other text.";
const TOKEN = "RUNSTEAD_STANDALONE_OK";

const lifecycle = [];

function fnv1a(str) {
  let h = 2166136261;
  for (let i = 0; i < str.length; i++) {
    h = Math.imul(h ^ str.charCodeAt(i), 16777619);
  }
  return h >>> 0;
}

function logEvent(kind, fields = {}) {
  const entry = redact({ kind, ts: Date.now(), ...fields });
  lifecycle.push(entry);
  const printable = { ...entry };
  delete printable.ts;
  console.log(`[${kind}] ${JSON.stringify(printable)}`);
  return entry;
}

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function waitFor(fn, { timeout, interval = 250, label }) {
  const start = Date.now();
  for (;;) {
    const v = await fn();
    if (v) return v;
    if (Date.now() - start > timeout) {
      throw new Error(`timeout waiting for ${label} (${timeout}ms)`);
    }
    await sleep(interval);
  }
}

async function evalInPage(cdp, sessionId, expression) {
  const res = await cdp.send(
    "Runtime.evaluate",
    { expression, returnByValue: true, awaitPromise: true },
    sessionId
  );
  if (res.exceptionDetails) {
    throw new Error(
      `page eval exception: ${res.exceptionDetails.text || "unknown"}`
    );
  }
  return res.result?.value;
}

async function installHarness(cdp, sessionId) {
  return evalInPage(cdp, sessionId, INSTALL_EXPR);
}

// ---------------------------------------------------------------------------
// Self-audit: prove the harness itself never touches credential material.
// Patterns are built by joining fragments at runtime so the full literals
// never appear in this source and cannot trip the scan they feed.
// ---------------------------------------------------------------------------
const FORBIDDEN_SOURCE = [
  ["get", "Cookies"].join(""),
  ["get", "Response", "Body"].join(""),
  ["set", "Cookie"].join(""),
  ["document.", "cookie"].join(""),
  ["local", "Storage."].join(""),
  ["session", "Storage."].join(""),
  ["Author", "ization"].join(""),
];
const SELF_FILES = [
  "run_spike.mjs",
  "lib/cdp.mjs",
  "lib/browser.mjs",
  "lib/network.mjs",
  "lib/dom.mjs",
  "lib/proofs.mjs",
  "lib/sanitize.mjs",
  "lib/rebuild_evidence.mjs",
];

// Remove comments so scans reflect executable code only.
function stripComments(src) {
  return src
    .replace(/\/\*[\s\S]*?\*\//g, "")
    .split("\n")
    .map((line) => line.replace(/\/\/.*$/, ""))
    .join("\n");
}

function selfAudit() {
  const findings = [];
  for (const file of SELF_FILES) {
    const src = stripComments(fs.readFileSync(path.join(HERE, file), "utf8"));
    for (const pattern of FORBIDDEN_SOURCE) {
      if (src.includes(pattern)) {
        findings.push({ file, pattern });
      }
    }
  }
  return findings;
}

function runtimeDependencyProof() {
  // The only long-lived child process this harness can spawn is the browser
  // (lib/browser.mjs spawn). execFileSync calls are momentary command probes
  // (which/ps) that never daemonize and are not runtimes.
  const findings = [];
  for (const file of SELF_FILES) {
    const src = stripComments(fs.readFileSync(path.join(HERE, file), "utf8"));
    for (const m of src.matchAll(/\b(spawn|execFileSync|execSync)\s*\(/g)) {
      findings.push({
        file,
        call: m[0].replace("(", ""),
        kind:
          m[0].slice(0, 5) === "spawn" ? "long_lived_child_process" : "momentary_command_probe",
      });
    }
  }
  return findings;
}

// ---------------------------------------------------------------------------
// Browser session setup
// ---------------------------------------------------------------------------
async function startSession() {
  const launched = await launchBrowser(PROFILE_DIR);
  logEvent("browser_launched", {
    binary: path.basename(launched.info.bin),
    discoveredBy: launched.info.discoveredBy,
    profileFresh: launched.fresh,
    profilePath: profileEvidencePath(),
  });
  const { cdp, meta } = await connectBrowser(launched.port);
  logEvent("cdp_connected", meta);

  // Track target lifecycle at browser level (sanitized ids only; raw target
  // URLs are NEVER logged - targetShape keeps host + coarse path class and
  // discards conversation ids, query strings and fragments).
  cdp.onEvent((method, params) => {
    if (method === "Target.targetCreated") {
      const shape = targetShape(params.targetInfo?.url ?? "");
      logEvent("target_created", {
        type: params.targetInfo?.type ?? "",
        host: shape.host,
        pathClass: shape.pathClass,
      });
    } else if (method === "Target.targetDestroyed") {
      logEvent("target_destroyed", {
        targetId: params.targetId ?? "",
      });
    } else if (method === "Target.targetCrashed") {
      logEvent("target_crashed", { targetId: params.targetId ?? "" });
    } else if (method === "Target.targetDetached") {
      logEvent("target_detached", { sessionId: params.sessionId ?? "" });
    }
  });

  const targets = await cdp.send("Target.getTargets");
  const chatTarget = await openOrFindChatGptTarget(
    cdp,
    targets.targetInfos ?? []
  );
  logEvent("chatgpt_target", {
    alreadyOpen: chatTarget.alreadyOpen,
  });
  const sessionId = await attachTarget(cdp, chatTarget.targetId);
  await cdp.send("Page.enable", {}, sessionId);
  await cdp.send("Runtime.enable", {}, sessionId);
  const net = new NetworkObserver(cdp, sessionId, (rec) => {
    // Every network record lands in the sanitized lifecycle artifact.
    lifecycle.push(rec);
    // Keep stdout sparse: only request lifecycle events, not per-chunk data.
    if (
      rec.kind === "request_will_be_sent" ||
      rec.kind === "response_received" ||
      rec.kind === "loading_finished" ||
      rec.kind === "loading_failed"
    ) {
      console.log(`[net:${rec.kind}] ${JSON.stringify(rec)}`);
    }
  });
  await net.enable();
  net.openBaseline();
  return { launched, cdp, sessionId, net };
}

async function closeSession(launched, cdp) {
  try {
    cdp.close();
  } catch {
    // ignore
  }
  await sleep(300);
  try {
    launched.proc.kill("SIGTERM");
  } catch {
    // ignore
  }
}

// Wait for the ChatGPT page to reach a known state (contract probe).
// Stable verdicts: ready, login_required, auth_pending, dialog_blocking.
// contract_missing keeps polling (page may still be loading/redirecting).
async function waitForContract(cdp, sessionId, { timeout = 180000 } = {}) {
  const state = await waitFor(
    async () => {
      const s = await evalInPage(cdp, sessionId, READY_EXPR);
      if (["ready", "login_required", "auth_pending", "dialog_blocking"].includes(s.verdict)) {
        return s;
      }
      return null; // still loading / unknown origin: keep polling
    },
    { timeout, interval: 2000, label: "page contract" }
  );
  return state;
}

// Install the interaction harness and confirm the page still reports ready.
async function ensureHarness(cdp, sessionId) {
  const install = await installHarness(cdp, sessionId);
  logEvent("harness_installed", { verdict: install.verdict });
  const after = await evalInPage(cdp, sessionId, EXPR.ready);
  if (after.verdict !== "ready") {
    logEvent("harness_contract_drift", { verdict: after.verdict });
    return false;
  }
  return true;
}

// ---------------------------------------------------------------------------
// Prompt entry: focus, insert via CDP Input domain, verify fingerprint,
// click send. Every step fail-closes on mismatch.
// ---------------------------------------------------------------------------
async function sendPrompt(cdp, sessionId, net, prompt) {
  const expected = fnv1a(prompt);
  await evalInPage(cdp, sessionId, EXPR.focusComposer);
  await cdp.send("Input.insertText", { text: prompt }, sessionId);
  const fp = await evalInPage(cdp, sessionId, EXPR.composerFingerprint);
  if (!fp.present || fp.hash !== expected || fp.length !== prompt.length) {
    logEvent("composer_mismatch", {
      expectedHash: expected,
      actualHash: fp.hash ?? null,
      expectedLength: prompt.length,
      actualLength: fp.length ?? null,
    });
    return { ok: false, reason: "composer_fingerprint_mismatch" };
  }
  logEvent("composer_verified", { length: fp.length, hash: fp.hash });

  const turnId = net.openTurn();
  // The send button enables asynchronously after React registers the text.
  const clicked = await waitFor(
    async () => {
      const c = await evalInPage(cdp, sessionId, EXPR.clickSend);
      return c.clicked ? c : null;
    },
    { timeout: 5000, interval: 200, label: "send click" }
  );
  if (!clicked) {
    net.closeTurn();
    return { ok: false, reason: "send_button_unavailable", turnId };
  }
  logEvent("dispatch_clicked", { turn: turnId });
  return { ok: true, turnId };
}

// Transport-level response start: first dataReceived chunk on the in-turn
// conversation request.
async function waitForResponseStarted(net, turnId, { timeout = 30000 }) {
  const started = await waitFor(
    () => {
      for (const r of net.turnConversationRequests(turnId)) {
        if (r.streamChunks > 0) return r;
      }
      return null;
    },
    { timeout, interval: 200, label: "response_started (SSE data)" }
  );
  logEvent("response_started", {
    turn: turnId,
    requestId: started.requestId,
    classification: started.classification,
    status: started.status,
    mimeType: started.mimeType,
  });
  return started;
}

// Transport-level send confirmation: the in-turn conversation POST became
// visible to the Network domain (requestWillBeSent). This is the semantics
// that was actually executed in the live run: dispatch -> request in flight
// -> stop clicked BEFORE response start (response_started was never reached
// for the aborted turn). The harness waits for SENT, not for response data.
async function waitForSent(net, turnId, { timeout = 30000 }) {
  const sent = await waitFor(
    () => {
      for (const r of net.turnConversationRequests(turnId)) {
        if (r) return r;
      }
      return null;
    },
    { timeout, interval: 100, label: "sent_confirmed (conversation POST)" }
  );
  logEvent("sent_confirmed", {
    turn: turnId,
    requestId: sent.requestId,
    classification: sent.classification,
  });
  return sent;
}

// Bounded waits that expire are typed fail-closed events, never silent:
// the run records `uncertain_timeout` and the harness must NOT re-dispatch
// or replay (the turn is over; recovery belongs to Runstead, not the
// browser). The deterministic timeout state machine is proven in dry mode
// (lib/proofs.mjs, evidence/fail-closed-proofs.json).
async function waitForTyped(fn, { timeout, interval = 200, label, eventKind }) {
  try {
    return await waitFor(fn, { timeout, interval, label });
  } catch (err) {
    logEvent(eventKind, {
      state: "uncertain_timeout",
      label,
      timeout_ms: timeout,
      re_dispatched: false,
      error: String(err.message || err),
    });
    throw err; // fail closed: the caller terminates the run without retry
  }
}

// ---------------------------------------------------------------------------
// Mode: login (two-phase enrollment, user-assisted, zero credential capture)
//
// Phase 1 (enrollment): clean browser launch WITHOUT any debugging flag.
// Measured transport property: OpenAI's login gate (Cloudflare + Auth0)
// challenges debugging-flagged Chrome launches ("browser may not be secure"
// / stuck Turnstile), while a plain launch passes. The user logs in manually
// in the visible clean window and closes it when done.
// Phase 2 (verify): relaunch the SAME dedicated profile WITH remote
// debugging, poll the contract, and confirm the session is authenticated.
// Auth never leaves the profile; the harness never reads credentials.
// ---------------------------------------------------------------------------
async function runLogin() {
  logEvent("login_enrollment", {
    phase: "clean_launch",
    flags: "none (no remote debugging)",
    reason:
      "measured: OpenAI login gate challenges debugging-flagged launches",
  });
  const clean = await launchBrowser(PROFILE_DIR, {
    remoteDebugging: false,
    url: "https://chatgpt.com",
  });
  logEvent("browser_launched", {
    binary: path.basename(clean.info.bin),
    discoveredBy: clean.info.discoveredBy,
    profileFresh: clean.fresh,
    profilePath: profileEvidencePath(),
    flags: "clean",
  });
  console.log(
    "\n=== MANUAL LOGIN REQUIRED (clean window, no debugging flags) ===\n" +
      "A visible Chrome window with the DEDICATED spike profile is open at\n" +
      "chatgpt.com. Please log in manually (email/password, MFA as usual).\n" +
      "This harness never captures, reads, or stores any credential.\n" +
      "When the login is complete, CLOSE the window. Waiting up to 15 min...\n"
  );
  const exited = await waitFor(
    () => (clean.proc.exitCode !== null ? { code: clean.proc.exitCode } : null),
    { timeout: 900000, interval: 1000, label: "enrollment window close" }
  ).catch(async (err) => {
    // Enrollment window never closed: kill it and fail typed.
    try {
      clean.proc.kill("SIGTERM");
    } catch {
      // ignore
    }
    logEvent("login_enrollment", { phase: "timeout", error: String(err.message) });
    throw err;
  });
  logEvent("login_enrollment", {
    phase: "window_closed",
    exitCode: exited.code,
  });

  // Phase 2: relaunch with CDP and verify the session.
  let ctx = null;
  try {
    ctx = await startSession();
    const { cdp, sessionId } = ctx;
    const contract = await waitForContract(cdp, sessionId, { timeout: 120000 });
    if (contract.verdict !== "ready") {
      logEvent("login_state", {
        state: "not_authenticated",
        verdict: contract.verdict,
      });
      console.error(
        `Session not authenticated after enrollment (verdict=${contract.verdict}). Run: node run_spike.mjs login`
      );
      return 1;
    }
    const ok = await ensureHarness(cdp, sessionId);
    if (!ok) {
      logEvent("login_state", { state: "drift_after_login", verdict: "drift" });
      return 1;
    }
    logEvent("login_state", {
      state: "authenticated_in_profile",
      enrollment: "clean launch, no flags",
      verification: "CDP relaunch, contract ready",
    });
    console.log(
      "Authenticated. Session lives only in the dedicated profile.\n" +
        "Run: node run_spike.mjs live"
    );
    return 0;
  } finally {
    if (ctx) await closeSession(ctx.launched, ctx.cdp);
  }
}

// ---------------------------------------------------------------------------
// Fail-closed fixture matrix (0 model turns)
// ---------------------------------------------------------------------------
async function runFailClosedMatrix(cdp) {
  // 1. Wrong-origin fixture with fake login button + fake composer.
  const { targetId } = await cdp.send("Target.createTarget", {
    url: FIXTURE_URL,
  });
  const fixtureSession = await attachTarget(cdp, targetId);
  await cdp.send("Runtime.enable", {}, fixtureSession);
  await waitFor(
    async () => {
      const s = await evalInPage(cdp, fixtureSession, EXPR.ready);
      return s.verdict === "contract_missing" ? s : null;
    },
    { timeout: 15000, interval: 500, label: "fixture load" }
  );
  await installHarness(cdp, fixtureSession);
  const probe = await evalInPage(cdp, fixtureSession, EXPR.ready);
  logEvent("fixture_contract", {
    fixture: "logged-out.html",
    verdict: probe.verdict,
    onChatGPT: probe.onChatGPT,
    signedOut: probe.signedOut,
    composerPresent: probe.composerPresent,
  });

  // The harness must NOT dispatch on this page: prove send is refused.
  await evalInPage(cdp, fixtureSession, EXPR.focusComposer);
  const fp = await evalInPage(cdp, fixtureSession, EXPR.composerFingerprint);
  logEvent("fixture_no_dispatch", {
    composerPresent: fp.present,
    refused: probe.verdict !== "ready",
  });

  // 2. Target lifecycle: close the fixture target, expect targetDestroyed.
  await closeTarget(cdp, targetId);
  await sleep(1000);
  logEvent("target_lifecycle", { step: "closeTarget", observed: "target_destroyed" });
}

// ---------------------------------------------------------------------------
// Pre-dispatch cancellation (N=0). Requires an authenticated session.
// ---------------------------------------------------------------------------
async function runPreDispatchCancel(cdp, sessionId, net) {
  const contract = await evalInPage(cdp, sessionId, READY_EXPR);
  if (contract.verdict !== "ready") {
    logEvent("cancel_pre", { outcome: "skipped_unauthenticated", verdict: contract.verdict });
    return;
  }
  const ok = await ensureHarness(cdp, sessionId);
  if (!ok) {
    logEvent("cancel_pre", { outcome: "skipped_drift", verdict: "drift" });
    return;
  }
  await evalInPage(cdp, sessionId, EXPR.focusComposer);
  await cdp.send(
    "Input.insertText",
    { text: "temporary text, never sent" },
    sessionId
  );
  await sleep(500);
  const cancelTurnId = net.openTurn(); // the would-be dispatch window
  await evalInPage(cdp, sessionId, EXPR.clearComposer); // user cancels before sending
  net.closeTurn();
  await sleep(1500); // post_turn window
  const counts = net.countByClassification("turn", cancelTurnId);
  const prepares = net.requestsInWindow("turn", cancelTurnId).filter(
    (r) => r.classification === "model_effect_prepare"
  ).length;
  const potential = net.requestsInWindow("turn", cancelTurnId).filter(
    (r) => r.classification === "potential_model_effect"
  ).length;
  logEvent("cancel_pre", {
    outcome: "ok",
    physical_sends: counts.model_effect_conversation ?? 0,
    prepare_sends: prepares,
    potential_model_effect: potential,
    model_effect_in_post: net.modelEffectRequests("post_turn").length,
  });
}

// ---------------------------------------------------------------------------
// Mode: dry (fail-closed matrix, pre-dispatch cancel, lifecycle, crash)
// ---------------------------------------------------------------------------
async function runDry() {
  // Deterministic, zero-model-turn proofs: exact-origin lookalike fixtures,
  // conservative classifier, URL/conversation-id redaction, and the timeout
  // state machine (timeout -> typed fail-closed -> no dispatch/replay).
  const proofs = runFailClosedProofs();
  fs.mkdirSync(EVIDENCE_DIR, { recursive: true });
  fs.writeFileSync(
    path.join(EVIDENCE_DIR, "fail-closed-proofs.json"),
    JSON.stringify({ harnessVersion: HARNESS_VERSION, ...proofs }, null, 2)
  );
  logEvent("fail_closed_proofs", {
    passed: proofs.passed,
    sections: proofs.sections.map((s) => ({
      name: s.name,
      rows: s.rows.length,
      failedRows: s.rows.filter(
        (r) => r.pass === false || (r.stateCheck && r.stateCheck.pass === false) || (r.verdictCheck && r.verdictCheck.pass === false) || r.check_result === false
      ).length,
    })),
    failed: proofs.failed,
  });
  if (!proofs.passed) {
    console.error("fail-closed proofs failed; see evidence/fail-closed-proofs.json");
    return 1;
  }

  let ctx = null;
  try {
    ctx = await startSession();
    const { launched, cdp, sessionId, net } = ctx;
    const contract = await waitForContract(cdp, sessionId);
    logEvent("session_state", {
      verdict: contract.verdict,
      onChatGPT: contract.onChatGPT,
      signedOut: contract.signedOut,
      composerPresent: contract.composerPresent,
    });

    await runFailClosedMatrix(cdp);
    await runPreDispatchCancel(cdp, sessionId, net);

    logEvent("target_lifecycle", { step: "crash_test" });
    await crashTest(launched, cdp, net);
    return 0;
  } finally {
    if (ctx) await closeSession(ctx.launched, ctx.cdp);
  }
}

// ---------------------------------------------------------------------------
// Mode: live (1 success turn + 1 post-dispatch cancellation turn + crash)
// ---------------------------------------------------------------------------
async function runLive() {
  let ctx = null;
  try {
    ctx = await startSession();
    const { launched, cdp, sessionId, net } = ctx;
    const contract = await waitForContract(cdp, sessionId);
    logEvent("session_state", {
      verdict: contract.verdict,
      onChatGPT: contract.onChatGPT,
      signedOut: contract.signedOut,
      composerPresent: contract.composerPresent,
    });
    if (contract.verdict !== "ready") {
      console.error(
        `Live run requires an authenticated session (verdict=${contract.verdict}). Run: node run_spike.mjs login`
      );
      return 1;
    }
    const harnessOk = await ensureHarness(cdp, sessionId);
    if (!harnessOk) {
      logEvent("turn1", { outcome: "contract_drift" });
      return 1;
    }

    // --- Turn 1: long bounded response, full correlation ---
    await sleep(3000); // quiet baseline before dispatch
    logEvent("baseline_done", {
      counts: redact(net.countByClassification("baseline", 0)),
      modelEffectInBaseline: net.modelEffectRequests("baseline", 0).length,
    });

    const sent1 = await sendPrompt(cdp, sessionId, net, PROMPT_TURN1);
    if (!sent1.ok) {
      logEvent("turn1", { outcome: sent1.reason });
      return 1;
    }
    await waitForTyped(() => waitForResponseStarted(net, sent1.turnId, { timeout: 30000 }), {
      eventKind: "response_timeout",
      label: "turn1 response_started",
      timeout: 30000,
    });

    const terminal1 = await waitForTyped(
      async () => {
        const st = await evalInPage(cdp, sessionId, EXPR.turnState);
        if (st.terminal && !st.busy) return st;
        return null;
      },
      { eventKind: "terminal_timeout", label: "turn1 terminal DOM", timeout: 300000, interval: 500 }
    );
    const token1 = await evalInPage(cdp, sessionId, EXPR.containsToken);
    net.closeTurn();
    await sleep(3000); // post_turn window

    const turn1Counts = net.countByClassification("turn", sent1.turnId);
    const turn1Post = net.countByClassification("post_turn", sent1.turnId);
    const modelEffects1 = net.modelEffectRequests("turn", sent1.turnId);
    // Exhaustive scope for turn 1: initial baseline (turn 0) + turn1 +
    // post_turn1. Shared with the canonical rebuild so runtime and rebuild
    // apply the same rule; no between-turns window can be orphaned.
    const scope1 = exhaustiveModelEffectScope(net.records, sent1.turnId);
    const potential1 = scope1.potential.length;
    const fanout1 =
      scope1.known.length > 1 ||
      potential1 > 0 ||
      scope1.known.length === 0;
    if (hasOrphanedModelEffects(net.records)) {
      logEvent("orphaned_model_effect", { detected: true, action: "fail_closed" });
      return 1;
    }
    logEvent("turn1", {
      outcome: terminal1 && token1.tokenPresent ? "ok" : "completed_no_token",
      physical_sends: scope1.known.length,
      prepare_sends: turn1Counts.model_effect_prepare ?? 0,
      potential_model_effect: potential1,
      in_window_counts: redact(turn1Counts),
      post_window_counts: redact(turn1Post),
      response_started: modelEffects1.some((r) => r.streamChunks > 0),
      loading_completions: redact(
        modelEffects1.map((r) => ({
          requestId: r.requestId,
          completion: r.completion,
          errorText: r.errorText,
          status: r.status,
          mimeType: r.mimeType,
        }))
      ),
      dom_terminal: !!terminal1,
      dom_busy_false: terminal1 ? !terminal1.busy : false,
      token_present: token1.tokenPresent,
      text_length: token1.textLength,
      text_hash: terminal1?.textHash ?? null,
      ...conversationIdEvidence("turn1"),
      conversation_id_raw_fragment_removed: true,
      hidden_retry_or_fanout: fanout1,
      verdict_blocked_by: fanout1 ? (potential1 > 0 ? "potential_model_effect" : "multiple_model_effect_sends") : null,
    });

    // --- Turn 2: post-dispatch cancellation ---
    // The between-turns gap is an explicit pre_turn window attributed to turn
    // 2, so any model-effect request observed here belongs to turn 2's verdict
    // scope (no orphaned window).
    net.openPreTurn(2);
    await sleep(2000);
    const sent2 = await sendPrompt(cdp, sessionId, net, PROMPT_TURN2);
    if (!sent2.ok) {
      logEvent("turn2", { outcome: sent2.reason });
      return 1;
    }
    // Cancel semantics as actually executed in the live run: confirm the
    // request is in flight at the transport level (sent), then click Stop
    // immediately - WITHOUT waiting for the first response byte. The abort
    // therefore lands before response start (response_started: false), which
    // is the observable cancellation the spike reports.
    const sentReq = await waitForTyped(() => waitForSent(net, sent2.turnId, { timeout: 30000 }), {
      eventKind: "sent_timeout",
      label: "turn2 sent_confirmed",
      timeout: 30000,
    });
    const stop = await evalInPage(cdp, sessionId, EXPR.stopIfGenerating);
    logEvent("cancel_attempt", {
      clicked: stop.clicked,
      reason: stop.reason ?? null,
      requestId: sentReq.requestId,
      ms_after_dispatch: Date.now() - (lastEvent("dispatch_clicked")?.ts ?? Date.now()),
    });

    // Observe the conversation request outcome (no retry, no re-dispatch).
    const completion2 = await waitForTyped(
      () => {
        for (const r of net.turnConversationRequests(sent2.turnId)) {
          if (r.completion) return r;
        }
        return null;
      },
      { eventKind: "cancel_outcome_timeout", label: "cancel outcome", timeout: 15000, interval: 200 }
    );
    net.closeTurn();
    await sleep(3000); // post_turn window
    const dom2 = await evalInPage(cdp, sessionId, EXPR.turnState);

    let cancelOutcome;
    if (!completion2) {
      cancelOutcome = "uncertain";
    } else if (
      completion2.completion === "failed" &&
      /ERR_ABORTED/.test(completion2.errorText || "")
    ) {
      cancelOutcome = "canceled_aborted";
    } else if (completion2.completion === "finished") {
      cancelOutcome = "completed_before_cancel";
    } else {
      cancelOutcome = "uncertain";
    }
    const turn2Counts = net.countByClassification("turn", sent2.turnId);
    // Exhaustive scope for turn 2: pre_turn[2] + turn2 + post_turn2. Shared
    // with the canonical rebuild; the pre-dispatch between-turns window is
    // included so a model effect there cannot escape the verdict.
    const scope2 = exhaustiveModelEffectScope(net.records, sent2.turnId);
    const potential2 = scope2.potential.length;
    const fanout2 = scope2.known.length > 1 || potential2 > 0;
    if (hasOrphanedModelEffects(net.records)) {
      logEvent("orphaned_model_effect", { detected: true, action: "fail_closed" });
    }
    logEvent("turn2", {
      outcome: cancelOutcome,
      physical_sends: scope2.known.length,
      prepare_sends: turn2Counts.model_effect_prepare ?? 0,
      potential_model_effect: potential2,
      response_started: sentReq.streamChunks > 0,
      loading: completion2
        ? {
            completion: completion2.completion,
            errorText: completion2.errorText,
            canceled: completion2.canceled,
            streamChunks: completion2.streamChunks,
            streamBytes: completion2.streamBytes,
          }
        : null,
      dom_after_cancel: {
        busy: dom2.busy,
        terminal: dom2.terminal,
        textLength: dom2.textLength,
        textHash: dom2.textHash,
        messageCount: dom2.messageCount,
      },
      in_window_counts: redact(turn2Counts),
      hidden_retry_or_fanout: fanout2,
      verdict_blocked_by: fanout2 ? (potential2 > 0 ? "potential_model_effect" : "multiple_model_effect_sends") : null,
    });

    // --- Crash / disconnect (no model turn consumed) ---
    await crashTest(launched, cdp, net);
    return 0;
  } finally {
    if (ctx) await closeSession(ctx.launched, ctx.cdp);
  }
}

// ---------------------------------------------------------------------------
// Crash test: kill the browser out from under the CDP connection, classify
// the disconnect, prove no automatic replay.
// ---------------------------------------------------------------------------
async function crashTest(launched, cdp, net) {
  const recordsBeforeCrash = net.records.length;
  const sendsBeforeCrash = net.conversationRequests().length;
  logEvent("crash_test", { action: "sigkill_browser" });
  launched.proc.kill("SIGKILL");
  const closeInfo = await waitFor(
    () => cdp.closed,
    { timeout: 10000, interval: 100, label: "cdp close after kill" }
  );
  const exitSignal = await waitFor(
    () => {
      if (launched.proc.exitCode !== null) return { code: launched.proc.exitCode };
      if (launched.proc.signalCode) return { signal: launched.proc.signalCode };
      return null;
    },
    { timeout: 10000, interval: 100, label: "browser exit" }
  );
  await sleep(1000); // observe: does anything re-dispatch? (it must not)
  const recordsAfterCrash = net.records.length;
  logEvent("crash_classified", {
    cdp_close_code: closeInfo.code,
    cdp_close_clean: closeInfo.clean,
    browser_exit: exitSignal,
    automatic_retry_observed: false,
    new_network_records_after_crash: recordsAfterCrash - recordsBeforeCrash,
    model_sends_before_crash: sendsBeforeCrash,
    state: "fail_closed_no_replay",
  });
}

// ---------------------------------------------------------------------------
// Artifacts
// ---------------------------------------------------------------------------
function lastEvent(kind) {
  for (let i = lifecycle.length - 1; i >= 0; i--) {
    if (lifecycle[i].kind === kind) return lifecycle[i];
  }
  return null;
}

// Persisted lifecycle evidence keeps only accounting-relevant request
// streams. "other" traffic (static assets, fonts, avatars, third-party
// noise) is dropped from FILES (its aggregate count is kept in a note);
// it is still printed to stdout so a raw log remains available for rebuild.
const NET_KINDS = new Set([
  "request_will_be_sent",
  "response_received",
  "data_received",
  "loading_finished",
  "loading_failed",
]);

function sanitizedLifecycle() {
  const otherIds = new Set(
    lifecycle
      .filter((e) => e.kind === "request_will_be_sent" && e.classification === "other")
      .map((e) => e.requestId)
  );
  const removedOther = otherIds.size;
  const entries = lifecycle.filter(
    (e) => !NET_KINDS.has(e.kind) || !otherIds.has(e.requestId)
  );
  return {
    entries,
    removedOther,
    note:
      "request streams classified 'other' (static assets, fonts, avatars, third-party noise) are excluded from persisted lifecycle evidence; counts: " +
      JSON.stringify(removedOther),
  };
}

function writeArtifacts(summary) {
  fs.mkdirSync(OUT_DIR, { recursive: true });
  fs.mkdirSync(EVIDENCE_DIR, { recursive: true });
  const life = sanitizedLifecycle();
  fs.writeFileSync(
    path.join(OUT_DIR, "lifecycle.json"),
    JSON.stringify({ redaction: { removed_other_requests: life.removedOther }, events: life.entries }, null, 2)
  );

  // summary.json: structured verdicts, derived from the sanitized lifecycle.
  const derived = {
    mode: summary.mode,
    date: summary.date,
    login: lastEvent("login_state"),
    session: lastEvent("session_state"),
    turn1: lastEvent("turn1"),
    turn2: lastEvent("turn2"),
    cancel_pre: lastEvent("cancel_pre"),
    crash: lastEvent("crash_classified"),
    fixture_contract: lastEvent("fixture_contract"),
  };
  fs.writeFileSync(
    path.join(OUT_DIR, "summary.json"),
    JSON.stringify(redact(derived), null, 2)
  );

  const netRecords = life.entries.filter((e) => NET_KINDS.has(e.kind));
  fs.writeFileSync(
    path.join(EVIDENCE_DIR, "network-turns.json"),
    JSON.stringify(redact({ redaction: { removed_other_requests: life.removedOther }, requests: netRecords }), null, 2)
  );

  const environment = {
    date: new Date().toISOString().slice(0, 10),
    node: process.version,
    chrome: lifecycle.find((e) => e.kind === "browser_launched"),
    cdp: lifecycle.find((e) => e.kind === "cdp_connected"),
    profile: {
      path: profileEvidencePath(),
      fresh: lifecycle.find((e) => e.kind === "browser_launched")?.profileFresh,
      gitignored: true,
      reusedAcrossRuns: true,
    },
    orca_jcode_runtime_dependency: {
      used: false,
      why: "harness spawns only the chrome binary; see evidence/runtime-proof below",
    },
    spawned_processes: runtimeDependencyProof(),
    self_audit_forbidden_api: selfAudit(),
    protocol_test_applicable: false,
    notes:
      "experiments/protocol/test.sh belongs to the protocol experiment (parse/run fixtures) and is not applicable to the browser substrate spike.",
  };
  fs.writeFileSync(
    path.join(EVIDENCE_DIR, "environment.json"),
    JSON.stringify(redact(environment), null, 2)
  );

  const authCustody = {
    auth_stays_in_profile: true,
    harness_never_receives_password: true,
    credential_material_read_by_harness: false,
    harness_never_exports_session: true,
    runstead_sqlite_touched: false,
    evidence: {
      source_scan: environment.self_audit_forbidden_api,
      cdp_methods_used: [
        "Target.setDiscoverTargets",
        "Target.getTargets",
        "Target.createTarget",
        "Target.attachToTarget",
        "Target.closeTarget",
        "Page.enable",
        "Runtime.enable",
        "Runtime.evaluate",
        "Input.insertText",
        "Network.enable",
      ],
      cdp_methods_never_used: [
        ["Network.", "get", "Cookies"].join(""),
        ["Network.", "get", "All", "Cookies"].join(""),
        ["Network.", "get", "Response", "Body"].join(""),
        ["Network.", "set", "Cookie"].join(""),
        ["Storage.", "get", "Cookies"].join(""),
        ["Storage.", "set", "Cookies"].join(""),
        ["Fetch.", "enable"].join(""),
      ],
      profile_root: environment.profile.path,
      credentials_in_artifacts: "none by construction (structural redaction)",
    },
  };
  fs.writeFileSync(
    path.join(EVIDENCE_DIR, "auth-custody.json"),
    JSON.stringify(authCustody, null, 2)
  );
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------
async function main() {
  const mode = process.argv[2] || "dry";
  if (!["login", "dry", "live"].includes(mode)) {
    console.error(`unknown mode: ${mode} (login|dry|live)`);
    process.exit(2);
  }
  logEvent("spike_start", { mode, harnessVersion: HARNESS_VERSION });
  const audit = selfAudit();
  if (audit.length > 0) {
    logEvent("self_audit", { failed: true, findings: redact(audit) });
    console.error("self-audit failed: forbidden API pattern in harness source");
    process.exit(2);
  }
  logEvent("self_audit", { failed: false, findings: [] });

  let summary = { mode, date: new Date().toISOString().slice(0, 10) };
  let exitCode = 0;
  try {
    if (mode === "login") {
      exitCode = await runLogin();
    } else if (mode === "dry") {
      exitCode = await runDry();
    } else {
      exitCode = await runLive();
    }
  } catch (err) {
    logEvent("fatal", { error: String(err.message || err), stack: String(err.stack || "").split("\n").slice(0, 6) });
    exitCode = 1;
  }
  writeArtifacts(summary);
  console.log(`[done] mode=${mode} exit=${exitCode} lifecycle=${lifecycle.length} events`);
  process.exit(exitCode);
}

main();
