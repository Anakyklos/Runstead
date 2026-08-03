#!/usr/bin/env bash
set -Eeuo pipefail

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
PARSE_SCRIPT="$ROOT_DIR/parse.sh"
RUN_SCRIPT="$ROOT_DIR/run.sh"
FIXTURES_DIR="$ROOT_DIR/fixtures/corpus"
TEST_TMP=$(mktemp -d "${TMPDIR:-/tmp}/runstead-protocol-test.XXXXXX")
trap 'rm -rf "$TEST_TMP"' EXIT

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

assert_jq() {
  local file=$1
  local expression=$2
  jq -e "$expression" "$file" >/dev/null || fail "$expression ($file)"
}

assert_parse() {
  local fixture=$1
  local expression=$2
  local output="$TEST_TMP/${fixture}.json"

  "$PARSE_SCRIPT" "$FIXTURES_DIR/$fixture.txt" >"$output" || fail "parser exited for $fixture"
  assert_jq "$output" "$expression"
}

assert_parse valid_action '.kind == "action" and .schema_valid and .executable and (.tool == "read_file")'
assert_parse valid_final '.kind == "final" and .schema_valid and .executable'
assert_parse mixed_prose_action '.kind == "action" and .schema_valid and .executable and .mixed_prose'
assert_parse malformed_json '.kind == "invalid" and .reason == "malformed_json" and (.executable | not)'
assert_parse invalid_schema '.kind == "action" and .reason == "invalid_action_schema" and (.executable | not)'
assert_parse unknown_tool '.kind == "action" and .schema_valid and (.reason == "unknown_tool") and (.executable | not)'
assert_parse protocol_refusal '.kind == "invalid" and .reason == "protocol_refusal"'
assert_parse unsupported_claim '.kind == "invalid" and .reason == "unsupported_execution_claim"'

REPORT_DIR="$TEST_TMP/offline"
"$RUN_SCRIPT" --offline --sessions 4 --output "$REPORT_DIR" >/dev/null
assert_jq "$REPORT_DIR/report.json" '.mode == "offline"'
assert_jq "$REPORT_DIR/report.json" '.sessions.completed >= 3'
assert_jq "$REPORT_DIR/report.json" '.protocol.tool_turns_completed >= 5'
assert_jq "$REPORT_DIR/report.json" '.protocol.corpus_cases >= 7'
assert_jq "$REPORT_DIR/report.json" '.protocol.correction_limit == 2'
assert_jq "$REPORT_DIR/report.json" '.protocol.correction_successes >= 3'
assert_jq "$REPORT_DIR/report.json" '.protocol.repeated_actions >= 1'
assert_jq "$REPORT_DIR/report.json" '.transport.failures == 0'
if grep -R '<fixture-placeholder>' "$REPORT_DIR" >/dev/null; then
  fail 'sanitized output retained a credential-shaped fixture value'
fi
grep -F '<redacted>' "$REPORT_DIR/corpus/secret_response.response.txt" >/dev/null || fail 'sanitizer did not redact the fixture credential'
grep -F '<redacted>' "$REPORT_DIR/corpus/secret_json.response.txt" >/dev/null || fail 'sanitizer did not redact JSON fixture credentials'
if grep -F '"./' "$REPORT_DIR/sessions/session-01/turn-02.observation.json" >/dev/null; then
  fail 'list_files emitted unstable ./ paths'
fi

ERROR_REPORT="$TEST_TMP/offline-tool-error"
set +e
"$RUN_SCRIPT" --offline --sessions 5 --tool-turns 5 --output "$ERROR_REPORT" >/dev/null
run_status=$?
set -e
[[ $run_status -eq 1 ]] || fail "expected one failed session (exit 1), got $run_status"
assert_jq "$ERROR_REPORT/report.json" '.sessions.completed == 4 and .sessions.failed == 1'
assert_jq "$ERROR_REPORT/report.json" '.sessions.completed_with_required_tool_turns == 4'
assert_jq "$ERROR_REPORT/report.json" '.protocol.tool_turns_failed == 1'

printf 'PASS: protocol parser and offline experiment checks\n'
