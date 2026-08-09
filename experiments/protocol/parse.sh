#!/usr/bin/env bash
set -Eeuo pipefail

PROTOCOL_VERSION="runstead.protocol.v1"

usage() {
  printf 'Usage: %s RESPONSE.txt\n' "$0"
}

if [[ ${1:-} == "--help" || $# -eq 0 ]]; then
  usage
  [[ $# -eq 0 ]] && exit 2
  exit 0
fi

INPUT_FILE=$1
[[ -f "$INPUT_FILE" ]] || { printf 'response file not found: %s\n' "$INPUT_FILE" >&2; exit 2; }

count_marker() {
  local marker=$1
  local count
  count=$( (grep -oF "$marker" "$INPUT_FILE" || true) | wc -l | tr -d '[:space:]')
  printf '%s\n' "$count"
}

extract_block() {
  local open=$1
  local close=$2

  awk -v open="$open" -v close_marker="$close" '
    {
      line = $0
      while (1) {
        if (!active) {
          start = index(line, open)
          if (!start) break
          line = substr(line, start + length(open))
          active = 1
        }

        finish = index(line, close_marker)
        if (!finish) {
          print line
          break
        }

        print substr(line, 1, finish - 1)
        line = substr(line, finish + length(close_marker))
        active = 0
      }
    }
    END {
      if (active) exit 3
    }
  ' "$INPUT_FILE"
}

outside_block() {
  local open=$1
  local close=$2

  awk -v open="$open" -v close_marker="$close" '
    {
      line = $0
      while (1) {
        if (!active) {
          start = index(line, open)
          if (!start) {
            if (line != "") print line
            break
          }
          prefix = substr(line, 1, start - 1)
          if (prefix != "") print prefix
          line = substr(line, start + length(open))
          active = 1
        }

        finish = index(line, close_marker)
        if (!finish) break
        line = substr(line, finish + length(close_marker))
        active = 0
      }
    }
    END {
      if (active) exit 3
    }
  ' "$INPUT_FILE"
}

invalid() {
  local reason=$1
  local mixed=${2:-false}
  jq -cn \
    --arg version "$PROTOCOL_VERSION" \
    --arg reason "$reason" \
    --argjson mixed "$mixed" \
    '{protocol_version:$version,kind:"invalid",schema_valid:false,executable:false,reason:$reason,mixed_prose:$mixed}'
}

ACTION_OPEN='<runstead_action>'
ACTION_CLOSE='</runstead_action>'
FINAL_OPEN='<runstead_final>'
FINAL_CLOSE='</runstead_final>'

action_open_count=$(count_marker "$ACTION_OPEN")
action_close_count=$(count_marker "$ACTION_CLOSE")
final_open_count=$(count_marker "$FINAL_OPEN")
final_close_count=$(count_marker "$FINAL_CLOSE")
envelope_count=$((action_open_count + final_open_count))

if (( envelope_count == 0 )); then
  if grep -Eiq "(^|[[:space:]])(I|we) (cannot|can't|am unable|refuse|won't|do not have access|don't have access)" "$INPUT_FILE"; then
    invalid "protocol_refusal"
  elif grep -Eiq "(I (have )?(read|listed|ran|executed|inspected|checked)|successfully (read|listed|ran|executed)|the file .* contains)" "$INPUT_FILE"; then
    invalid "unsupported_execution_claim"
  else
    invalid "missing_envelope"
  fi
  exit 0
fi

if (( envelope_count != 1 )); then
  invalid "multiple_envelopes"
  exit 0
fi

if (( action_open_count == 1 && action_close_count != 1 )) || (( final_open_count == 1 && final_close_count != 1 )); then
  invalid "unclosed_envelope"
  exit 0
fi

if (( action_open_count == 1 && final_open_count == 1 )); then
  invalid "multiple_envelopes"
  exit 0
fi

if (( action_open_count == 1 )); then
  envelope_kind=action
  block_file=$(mktemp "${TMPDIR:-/tmp}/runstead-action.XXXXXX")
  trap 'rm -f "$block_file"' EXIT
  extract_block "$ACTION_OPEN" "$ACTION_CLOSE" >"$block_file" || { invalid "unclosed_envelope"; exit 0; }
  open_marker="$ACTION_OPEN"
  close_marker="$ACTION_CLOSE"
else
  envelope_kind=final
  block_file=$(mktemp "${TMPDIR:-/tmp}/runstead-final.XXXXXX")
  trap 'rm -f "$block_file"' EXIT
  extract_block "$FINAL_OPEN" "$FINAL_CLOSE" >"$block_file" || { invalid "unclosed_envelope"; exit 0; }
  open_marker="$FINAL_OPEN"
  close_marker="$FINAL_CLOSE"
fi

outside_file=$(mktemp "${TMPDIR:-/tmp}/runstead-outside.XXXXXX")
trap 'rm -f "$block_file" "$outside_file"' EXIT
outside_block "$open_marker" "$close_marker" >"$outside_file" || { invalid "unclosed_envelope"; exit 0; }
if grep -q '[^[:space:]]' "$outside_file"; then
  mixed_prose=true
else
  mixed_prose=false
fi

if ! jq -e -s 'length == 1' "$block_file" >/dev/null 2>&1; then
  invalid "malformed_json" "$mixed_prose"
  exit 0
fi

if [[ $envelope_kind == action ]]; then
  if ! jq -e -s --arg version "$PROTOCOL_VERSION" '
      length == 1 and
      (.[0] | type == "object") and
      (.[0].version? == $version) and
      (.[0].tool? | type == "string") and
      (.[0].arguments? | type == "object") and
      ((.[0] | keys | sort) == ["arguments", "tool", "version"])
    ' "$block_file" >/dev/null 2>&1; then
    jq -cn \
      --arg version "$PROTOCOL_VERSION" \
      --argjson mixed "$mixed_prose" \
      '{protocol_version:$version,kind:"action",schema_valid:false,executable:false,reason:"invalid_action_schema",mixed_prose:$mixed}'
    exit 0
  fi

  tool=$(jq -r -s '.[0].tool' "$block_file")
  arguments=$(jq -c -s '.[0].arguments' "$block_file")
  if [[ $tool == read_file || $tool == list_files ]]; then
    reason=ok
    executable=true
  else
    reason=unknown_tool
    executable=false
  fi
  jq -cn \
    --arg version "$PROTOCOL_VERSION" \
    --arg tool "$tool" \
    --arg reason "$reason" \
    --argjson arguments "$arguments" \
    --argjson executable "$executable" \
    --argjson mixed "$mixed_prose" \
    '{protocol_version:$version,kind:"action",schema_valid:true,executable:$executable,reason:$reason,mixed_prose:$mixed,tool:$tool,arguments:$arguments}'
else
  if ! jq -e -s --arg version "$PROTOCOL_VERSION" '
      length == 1 and
      (.[0] | type == "object") and
      (.[0].version? == $version) and
      (.[0].status? | type == "string") and
      (.[0].status == "complete" or .[0].status == "incomplete") and
      (.[0].summary? | type == "string") and
      (.[0].evidence? | type == "array") and
      (.[0].evidence | length > 0) and
      (.[0].evidence | all(.[];
        (type == "object") and
        (.["evidence_id"]? | type == "string") and
        (.["tool"]? | type == "string"))) and
      ((.[0] | keys | sort) == ["evidence", "status", "summary", "version"])
    ' "$block_file" >/dev/null 2>&1; then
    jq -cn \
      --arg version "$PROTOCOL_VERSION" \
      --argjson mixed "$mixed_prose" \
      '{protocol_version:$version,kind:"final",schema_valid:false,executable:false,reason:"invalid_final_schema",mixed_prose:$mixed}'
    exit 0
  fi

  status=$(jq -r -s '.[0].status' "$block_file")
  summary=$(jq -r -s '.[0].summary' "$block_file")
  evidence=$(jq -c -s '.[0].evidence' "$block_file")
  jq -cn \
    --arg version "$PROTOCOL_VERSION" \
    --arg status "$status" \
    --arg summary "$summary" \
    --argjson evidence "$evidence" \
    --argjson mixed "$mixed_prose" \
    '{protocol_version:$version,kind:"final",schema_valid:true,executable:true,reason:"ok",mixed_prose:$mixed,status:$status,summary:$summary,evidence:$evidence}'
fi
