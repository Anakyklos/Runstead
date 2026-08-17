#!/usr/bin/env bash
# test.sh - deterministic validation for the standalone first-party browser
# substrate spike. ZERO model turns, ZERO browser launch. Exercises the pure
# public surfaces (classifier, origin classifier, url/target shaping,
# conversation-id redaction, timeout state machine) and verifies the canonical
# live-evidence rebuild is idempotent and internally consistent.
#
# Usage: ./test.sh   (from anywhere; resolves its own directory)
set -Eeuo pipefail

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
TEST_TMP=$(mktemp -d "${TMPDIR:-/tmp}/runstead-standalone-test.XXXXXX")
trap 'rm -rf "$TEST_TMP"' EXIT

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

# 1. Deterministic fail-closed proofs (origin/classifier/redaction/timeout).
node --input-type=module -e '
import { runFailClosedProofs } from "file://'$ROOT_DIR'/lib/proofs.mjs";
const p = runFailClosedProofs();
if (!p.passed) {
  console.error(JSON.stringify(p.failed, null, 2));
  process.exit(1);
}
console.log("proofs passed:", p.sections.length, "sections");
' || fail "deterministic fail-closed proofs"

# 2. Conservative classifier edge cases beyond the fixture table.
node --input-type=module -e '
import { classifyRequest } from "file://'$ROOT_DIR'/lib/network.mjs";
import { sanitizeConversationPath } from "file://'$ROOT_DIR'/lib/sanitize.mjs";
const cases = [
  ["POST","/backend-api/conversation/abcdef12-3456-7890-abcd-ef1234567890/stream_status","potential_model_effect"],
  ["PUT","/backend-api/conversation/abcdef12/stream_status","conversation_api_aux"],  // non-POST not a model effect
  ["POST","/backend-api/f/conversation/branch","potential_model_effect"],             // unknown f-route
  ["GET","/backend-api/conversation/abcdef12","conversation_api_aux"],
];
for (const [m,p,exp] of cases) {
  const got = classifyRequest(m,p);
  if (got !== exp) { console.error(`classify ${m} ${p} = ${got}, want ${exp}`); process.exit(1); }
}
const p = sanitizeConversationPath("/backend-api/conversation/abcdef12-3456-7890-abcd-ef1234567890/stream_status");
if (p !== "/backend-api/conversation/<conv>/stream_status") { console.error("sanitize path =", p); process.exit(1); }
console.log("classifier edge cases passed:", cases.length, "+ path sanitize");
' || fail "classifier edge cases"

# 3. URL / target shaping + conversation-id redaction public interface.
node --input-type=module -e '
import { urlShape, targetShape, conversationIdEvidence, redact } from "file://'$ROOT_DIR'/lib/sanitize.mjs";
const u = urlShape("https://chatgpt.com/c/6a811a31-6f7a-4f6a-9c81-6f7a4f6a9c81?token=secret#frag");
if (u.host !== "chatgpt.com" || /\?|#|6a811a31/.test(u.path)) { console.error("urlShape=", u); process.exit(1); }
const t = targetShape("https://chatgpt.com/c/6a811a31-6f7a-4f6a-9c81-6f7a4f6a9c81?x=1#f");
if (t.pathClass !== "/c" || JSON.stringify(t).includes("6a811a31")) { console.error("targetShape=", t); process.exit(1); }
const tf = targetShape("file:///some/local/fixture/path");
if (tf.host !== "file:" || tf.pathClass !== "/") { console.error("file targetShape=", tf); process.exit(1); }
const ev = redact(conversationIdEvidence("turn1"));
if (ev.conversation_id !== null || ev.conversation_id_placeholder !== "conv#turn1" || JSON.stringify(ev).includes("6a811a31")) { console.error("conv evidence=", ev); process.exit(1); }
console.log("redaction shaping passed: url/target/file/conv-id");
' || fail "redaction shaping"

# 4. Canonical live-evidence rebuild is idempotent + internally consistent.
node "$ROOT_DIR/lib/rebuild_evidence.mjs" >/dev/null || fail "rebuild (pass 1)"
cp "$ROOT_DIR/evidence/live-key-events.json" "$TEST_TMP/ke1.json"
cp "$ROOT_DIR/output/summary-live.json"      "$TEST_TMP/sum1.json"
node "$ROOT_DIR/lib/rebuild_evidence.mjs" >/dev/null || fail "rebuild (pass 2)"
diff -q "$TEST_TMP/ke1.json" "$ROOT_DIR/evidence/live-key-events.json" >/dev/null || fail "rebuild not idempotent (key events)"
diff -q "$TEST_TMP/sum1.json" "$ROOT_DIR/output/summary-live.json" >/dev/null || fail "rebuild not idempotent (summary)"
# Cross-artifact agreement: key-event turn verdicts match summary verdicts.
node --input-type=module -e '
import { readFileSync } from "node:fs";
const base = "'$ROOT_DIR'";
const ke = JSON.parse(readFileSync(base + "/evidence/live-key-events.json","utf8")).events;
const sum = JSON.parse(readFileSync(base + "/output/summary-live.json","utf8"));
const t1 = ke.find(e=>e._kind==="turn1");
const t2 = ke.find(e=>e._kind==="turn2");
if (sum.turn1.physical_sends !== t1.physical_sends) process.exit(1);
if (sum.turn2.outcome !== t2.outcome) process.exit(1);
if (sum.turn2.physical_sends !== t2.physical_sends) process.exit(1);
if (sum.turn1.response_started !== t1.response_started) process.exit(1);
console.log("cross-artifact agreement: key-events == summary");
' || fail "cross-artifact agreement"
# 5. Evidence-authority regression (review): an orphaned model-effect request
# must terminate the rebuild non-zero BEFORE any clean/final canonical verdict
# is persisted, and must never overwrite the valid canonical artifacts with
# apparently-clean results of an invalid run. Hermetic: runs the rebuild against
# a synthetic orphaned log in $TEST_TMP and verifies the three real canonical
# artifacts are byte-for-byte untouched.
ORPHAN_LOG="$TEST_TMP/orphaned-run.log"
cat >"$ORPHAN_LOG" <<'LOG'
[net:request_will_be_sent] {"requestId":"110.1","method":"POST","host":"chatgpt.com","path":"/backend-api/f/conversation","window":"turn","turn":1,"ts":1786845745125}
[net:request_will_be_sent] {"requestId":"110.2","method":"POST","host":"chatgpt.com","path":"/backend-api/f/conversation","window":"turn","turn":2,"ts":1786845754010}
[net:request_will_be_sent] {"requestId":"110.3","method":"POST","host":"chatgpt.com","path":"/backend-api/f/conversation","window":"turn","turn":0,"ts":1786845759000}
LOG
NET_LIVE_BEFORE=$(sha256sum "$ROOT_DIR/evidence/network-turns-live.json" | cut -d' ' -f1)
KE_LIVE_BEFORE=$(sha256sum "$ROOT_DIR/evidence/live-key-events.json" | cut -d' ' -f1)
SUM_LIVE_BEFORE=$(sha256sum "$ROOT_DIR/output/summary-live.json" | cut -d' ' -f1)
if node "$ROOT_DIR/lib/rebuild_evidence.mjs" "$ORPHAN_LOG" >/dev/null 2>&1; then
  fail "orphaned model-effect rebuild exited 0 (must terminate non-success)"
fi
NET_LIVE_AFTER=$(sha256sum "$ROOT_DIR/evidence/network-turns-live.json" | cut -d' ' -f1)
KE_LIVE_AFTER=$(sha256sum "$ROOT_DIR/evidence/live-key-events.json" | cut -d' ' -f1)
SUM_LIVE_AFTER=$(sha256sum "$ROOT_DIR/output/summary-live.json" | cut -d' ' -f1)
if [ "$NET_LIVE_BEFORE" != "$NET_LIVE_AFTER" ] || [ "$KE_LIVE_BEFORE" != "$KE_LIVE_AFTER" ] || [ "$SUM_LIVE_BEFORE" != "$SUM_LIVE_AFTER" ]; then
  fail "orphaned rebuild overwrote canonical artifacts (fail-open persistence)"
fi
echo "orphaned rebuild regression passed: non-zero exit + canonical artifacts untouched"
# No residual conversation-id fragment or third-party host in the DERIVED
# live/canonical artifacts. (evidence/fail-closed-proofs.json intentionally
# contains synthetic fixture IDs used to demonstrate the redaction rules and
# classifier, not real data; it is excluded here.)
DERIVED_ARTIFACTS=(
  "$ROOT_DIR/evidence/live-key-events.json"
  "$ROOT_DIR/evidence/network-turns-live.json"
  "$ROOT_DIR/evidence/network-turns.json"
  "$ROOT_DIR/output/summary-live.json"
  "$ROOT_DIR/output/summary-dry.json"
  "$ROOT_DIR/output/lifecycle-dry.json"
)
if grep -l "6a811a31" "${DERIVED_ARTIFACTS[@]}" 2>/dev/null; then
  fail "conversation-id fragment leaked into derived evidence/output"
fi
if grep -l "googleusercontent\|auth0.com/avatars" "${DERIVED_ARTIFACTS[@]}" 2>/dev/null; then
  fail "third-party/personal path leaked into derived evidence/output"
fi
# Hermeticity: derived evidence must never carry an absolute filesystem path
# (home dir or a machine-specific checkout path) for the profile or anywhere.
if grep -lE "/home/[^/]+|Documentos/codigo" "${DERIVED_ARTIFACTS[@]}" 2>/dev/null; then
  fail "absolute filesystem path leaked into derived evidence/output (hermeticity)"
fi

echo "OK: standalone spike deterministic tests passed"
