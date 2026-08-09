#!/usr/bin/env bash
set -Eeuo pipefail

ROOT_DIR=$(CDPATH= cd "$(dirname "$0")" && pwd)
PARSE_SCRIPT="$ROOT_DIR/parse.sh"
FIXTURES_DIR="$ROOT_DIR/fixtures"
PROTOCOL_VERSION="runstead.protocol.v1"
DEFAULT_SESSIONS=3
DEFAULT_TOOL_TURNS=5
DEFAULT_CORRECTION_RETRIES=2

MODE=offline
SESSIONS=$DEFAULT_SESSIONS
TOOL_TURNS=$DEFAULT_TOOL_TURNS
CORRECTION_LIMIT=$DEFAULT_CORRECTION_RETRIES
OUTPUT_DIR=

usage() {
  cat <<'EOF'
Usage: run.sh [--offline|--live] [options]

Modes:
  --offline                 Replay the committed transport-neutral fixtures (default).
  --live                    POST non-streaming requests to OmniRoute.

Options:
  --output DIR              Retain sanitized captures and the report in DIR.
  --sessions N              Independent sessions to run (default: 3).
  --tool-turns N            Successful read-only tool turns per session (default: 5).
  --correction-retries N    Maximum protocol corrections per session (default: 2).
  --help                    Show this help.

Live mode requires OMNIROUTE_BASE_URL, OMNIROUTE_API_KEY and OMNIROUTE_MODEL.
OMNIROUTE_CHAT_ENDPOINT may override the default chat completions URL.
EOF
}

die() {
  printf 'error: %s\n' "$*" >&2
  exit 2
}

while (($#)); do
  case $1 in
    --offline)
      MODE=offline
      shift
      ;;
    --live)
      MODE=live
      shift
      ;;
    --output)
      (($# >= 2)) || die '--output requires a directory'
      OUTPUT_DIR=$2
      shift 2
      ;;
    --sessions)
      (($# >= 2)) || die '--sessions requires a number'
      SESSIONS=$2
      shift 2
      ;;
    --tool-turns)
      (($# >= 2)) || die '--tool-turns requires a number'
      TOOL_TURNS=$2
      shift 2
      ;;
    --correction-retries)
      (($# >= 2)) || die '--correction-retries requires a number'
      CORRECTION_LIMIT=$2
      shift 2
      ;;
    --help)
      usage
      exit 0
      ;;
    *)
      die "unknown option: $1"
      ;;
  esac
done

[[ $SESSIONS =~ ^[1-9][0-9]*$ ]] || die '--sessions must be a positive integer'
[[ $TOOL_TURNS =~ ^[1-9][0-9]*$ ]] || die '--tool-turns must be a positive integer'
[[ $CORRECTION_LIMIT =~ ^[0-9]+$ ]] || die '--correction-retries must be a non-negative integer'

command -v jq >/dev/null 2>&1 || die 'jq is required'
command -v awk >/dev/null 2>&1 || die 'awk is required'
command -v find >/dev/null 2>&1 || die 'find is required'

if [[ $MODE == live ]]; then
  command -v curl >/dev/null 2>&1 || die 'curl is required for --live'
  [[ -n ${OMNIROUTE_BASE_URL:-} ]] || die 'OMNIROUTE_BASE_URL is required for --live'
  [[ -n ${OMNIROUTE_API_KEY:-} ]] || die 'OMNIROUTE_API_KEY is required for --live'
  [[ -n ${OMNIROUTE_MODEL:-} ]] || die 'OMNIROUTE_MODEL is required for --live'
  (( SESSIONS >= DEFAULT_SESSIONS )) || die "--live requires at least $DEFAULT_SESSIONS independent sessions"
  (( TOOL_TURNS >= DEFAULT_TOOL_TURNS )) || die "--live requires at least $DEFAULT_TOOL_TURNS tool turns"
  OMNIROUTE_CHAT_ENDPOINT=${OMNIROUTE_CHAT_ENDPOINT:-${OMNIROUTE_BASE_URL%/}/chat/completions}
else
  OMNIROUTE_MODEL=fixture-replay
fi

RUN_ID=$(date -u +%Y%m%dT%H%M%SZ)-$$
if [[ -z $OUTPUT_DIR ]]; then
  OUTPUT_DIR="$ROOT_DIR/results/$RUN_ID"
fi
mkdir -p "$OUTPUT_DIR"
OUTPUT_DIR=$(CDPATH= cd "$OUTPUT_DIR" && pwd -P)
chmod 700 "$OUTPUT_DIR"
EVENTS_FILE="$OUTPUT_DIR/events.jsonl"
CORPUS_OUTPUT_DIR="$OUTPUT_DIR/corpus"
mkdir -p "$CORPUS_OUTPUT_DIR"

redact_text() {
  sed -E \
    -e 's/(Bearer[[:space:]]+)[^[:space:]"'"'"']+/\1<redacted>/Ig' \
    -e 's/sk-[A-Za-z0-9_-]{8,}/<redacted>/g' \
    -e 's/((api[_-]?key|authorization|token|password|secret)[[:space:]]*[:=][[:space:]]*)[^,;[:space:]"'"'"'}]+/\1<redacted>/Ig'
}

sanitize_file() {
  local source_file=$1
  local destination_file=$2
  local jq_redactor

  jq_redactor='
    def scrub:
      if type == "object" then
        to_entries
        | map(if (.key | test("(?i)(api[_-]?key|authorization|token|password|secret)"))
              then .value = "<redacted>"
              else .value |= scrub
              end)
        | from_entries
      elif type == "array" then map(scrub)
      elif type == "string" then
        gsub("(?i)Bearer[[:space:]]+[A-Za-z0-9._~+/=-]+"; "Bearer <redacted>")
        | gsub("sk-[A-Za-z0-9_-]{8,}"; "<redacted>")
        | gsub("(?i)(api[_-]?key|authorization|token|password|secret)[[:space:]]*[:=][[:space:]]*[^,;}[:space:]]+"; "<redacted>")
      else . end;
    scrub
  '

  if jq -e -s 'length == 1' "$source_file" >/dev/null 2>&1; then
    if jq -c -s ". [0] | $jq_redactor" "$source_file" >"$destination_file" 2>/dev/null; then
      return 0
    fi
  fi
  redact_text <"$source_file" >"$destination_file"
}

write_event() {
  printf '%s\n' "$1" >>"$EVENTS_FILE"
}

corpus_cases() {
  local manifest_line case_name fixture_file parser_tmp parser_saved response_saved event
  while IFS= read -r manifest_line || [[ -n $manifest_line ]]; do
    [[ -z $manifest_line ]] && continue
    case_name=$(jq -r '.case' <<<"$manifest_line")
    fixture_file=$(jq -r '.fixture' <<<"$manifest_line")
    parser_tmp=$(mktemp "${TMPDIR:-/tmp}/runstead-parser.XXXXXX")
    parser_saved="$CORPUS_OUTPUT_DIR/$case_name.parser.json"
    response_saved="$CORPUS_OUTPUT_DIR/$case_name.response.txt"
    "$PARSE_SCRIPT" "$FIXTURES_DIR/corpus/$fixture_file" >"$parser_tmp"
    sanitize_file "$parser_tmp" "$parser_saved"
    sanitize_file "$FIXTURES_DIR/corpus/$fixture_file" "$response_saved"
    event=$(jq -cn \
      --arg case_name "$case_name" \
      --arg fixture "$fixture_file" \
      --slurpfile parse "$parser_saved" \
      '{event:"corpus_case",case:$case_name,fixture:$fixture,parse:$parse[0]}')
    write_event "$event"
    rm -f "$parser_tmp"
  done <"$FIXTURES_DIR/corpus/manifest.jsonl"
}

is_safe_relative() {
  local path=$1
  [[ -n $path && $path != /* ]] || return 1
  case "/$path/" in
    */../*) return 1 ;;
  esac
}

workspace_target() {
  local relative=$1
  local parent base parent_real target
  is_safe_relative "$relative" || return 1
  parent=$(dirname "$relative")
  base=$(basename "$relative")
  parent_real=$(CDPATH= cd "$FIXTURES_DIR/workspace/$parent" 2>/dev/null && pwd -P) || return 1
  target="$parent_real/$base"
  case "$target" in
    "$WORKSPACE_REAL"/*|"$WORKSPACE_REAL") ;;
    *) return 1 ;;
  esac
  [[ -L $target ]] && return 1
  printf '%s\n' "$target"
}

tool_error() {
  local tool=$1
  local code=$2
  local message=$3
  jq -cn --arg tool "$tool" --arg code "$code" --arg message "$message" \
    '{ok:false,tool:$tool,error:{code:$code,message:$message}}'
}

read_file_tool() {
  local path=$1
  local target content bytes
  target=$(workspace_target "$path") || { tool_error read_file unsafe_path 'path is outside the fixture workspace'; return 0; }
  [[ -f $target ]] || { tool_error read_file not_a_file 'path does not identify a regular file'; return 0; }
  content=$(jq -Rs . <"$target")
  bytes=$(wc -c <"$target" | tr -d '[:space:]')
  jq -cn \
    --arg path "$path" \
    --argjson content "$content" \
    --argjson bytes "$bytes" \
    '{ok:true,tool:"read_file",result:{path:$path,content:$content,bytes:$bytes}}'
}

list_files_tool() {
  local path=$1
  local target files
  target=$(workspace_target "$path") || { tool_error list_files unsafe_path 'path is outside the fixture workspace'; return 0; }
  [[ -d $target ]] || { tool_error list_files not_a_directory 'path does not identify a directory'; return 0; }
  files=$(find "$target" -type f -print | while IFS= read -r file; do
    relative=${file#"$WORKSPACE_REAL"/}
    relative=${relative#./}
    printf '%s\n' "$relative"
  done | sort | jq -R -s 'split("\n") | map(select(length > 0))')
  jq -cn --arg path "$path" --argjson files "$files" \
    '{ok:true,tool:"list_files",result:{path:$path,files:$files}}'
}

execute_tool() {
  local outcome_file=$1
  local tool arguments path result
  tool=$(jq -r '.tool' "$outcome_file")
  arguments=$(jq -c '.arguments' "$outcome_file")
  case $tool in
    read_file)
      if ! jq -e '.path? | type == "string" and length > 0' <<<"$arguments" >/dev/null; then
        result=$(tool_error read_file invalid_arguments 'arguments.path must be a non-empty string')
      else
        path=$(jq -r '.path' <<<"$arguments")
        result=$(read_file_tool "$path")
      fi
      ;;
    list_files)
      path=$(jq -r '.path // "."' <<<"$arguments")
      if ! jq -e '.path? // "." | type == "string" and length > 0' <<<"$arguments" >/dev/null; then
        result=$(tool_error list_files invalid_arguments 'arguments.path must be a non-empty string')
      else
        result=$(list_files_tool "$path")
      fi
      ;;
    *)
      result=$(tool_error "$tool" unknown_tool 'tool is not enabled in the M0 experiment')
      ;;
  esac
  printf '%s\n' "$result"
}

request_body() {
  local messages=$1
  jq -cn \
    --arg model "$OMNIROUTE_MODEL" \
    --argjson messages "$messages" \
    '{model:$model,messages:$messages,stream:false}'
}

append_message() {
  local messages=$1
  local role=$2
  local content=$3
  jq -c --arg role "$role" --arg content "$content" '. + [{role:$role,content:$content}]' <<<"$messages"
}

invoke_live() {
  local request_file=$1
  local response_file=$2
  local headers_file=$3
  local curl_error_file=$4
  local curl_config http_code curl_status

  curl_config=$(mktemp "${TMPDIR:-/tmp}/runstead-curl.XXXXXX")
  chmod 600 "$curl_config"
  printf '%s\n' \
    "url = \"$OMNIROUTE_CHAT_ENDPOINT\"" \
    'request = "POST"' \
    'header = "Content-Type: application/json"' \
    "header = \"Authorization: Bearer $OMNIROUTE_API_KEY\"" \
    "header = \"X-Session-Id: $CURRENT_SESSION_ID\"" \
    "header = \"X-Request-Id: $RUN_ID-$CURRENT_SESSION_ID-$CURRENT_REQUEST\"" \
    'header = "X-OmniRoute-No-Cache: true"' \
    "data-binary = \"@$request_file\"" \
    >"$curl_config"

  set +e
  http_code=$(curl --silent --show-error --config "$curl_config" \
    --dump-header "$headers_file" \
    --output "$response_file" \
    --write-out '%{http_code}' \
    2>"$curl_error_file")
  curl_status=$?
  set -e
    rm -f "$curl_config"

  HTTP_STATUS=${http_code:-000}
  CURL_EXIT=$curl_status
  if (( curl_status != 0 )); then
    TRANSPORT_CLASS=transport_error
    return 1
  fi
  case $HTTP_STATUS in
    401|403) TRANSPORT_CLASS=auth_failure; return 1 ;;
    2[0-9][0-9]) TRANSPORT_CLASS=success; return 0 ;;
    *) TRANSPORT_CLASS=provider_http_error; return 1 ;;
  esac
}

normalize_provider_response() {
  local response_file=$1
  local model_content_file=$2
  if ! jq -e -s 'length == 1 and (.[0] | type == "object")' "$response_file" >/dev/null 2>&1; then
    PROVIDER_CLASS=provider_malformed_json
    return 1
  fi
  if jq -e -s '.[0].error? != null' "$response_file" >/dev/null 2>&1; then
    PROVIDER_CLASS=provider_error_envelope
    return 1
  fi
  if ! jq -e -s '
      length == 1 and
      (.[0].choices | type == "array") and
      ((.[0].choices[0].message.content? // null) | type == "string" and length > 0)
    ' "$response_file" >/dev/null 2>&1; then
    if jq -e -s '((.[0].choices[0].message.content? // null) | type == "string" and length == 0)' "$response_file" >/dev/null 2>&1; then
      PROVIDER_CLASS=provider_empty_response
    else
      PROVIDER_CLASS=provider_invalid_envelope
    fi
    return 1
  fi
  jq -r -s '.[0].choices[0].message.content' "$response_file" >"$model_content_file"
  PROVIDER_CLASS=success
}

write_captures() {
  local session_dir=$1
  local turn=$2
  local request_file=$3
  local response_file=$4
  local headers_file=$5
  local curl_error_file=$6
  local capture_base transport_file

  capture_base="$session_dir/turn-$(printf '%02d' "$turn")"
  sanitize_file "$request_file" "$capture_base.request.json"
  sanitize_file "$response_file" "$capture_base.response.txt"
  sanitize_file "$headers_file" "$capture_base.headers.txt"
  sanitize_file "$curl_error_file" "$capture_base.transport-error.txt"
  transport_file="$capture_base.transport.json"
  jq -cn \
    --arg class "$TRANSPORT_CLASS" \
    --arg http_status "$HTTP_STATUS" \
    --argjson curl_exit "$CURL_EXIT" \
    '{schema:"runstead.protocol.transport.v1",classification:$class,http_status:$http_status,curl_exit:$curl_exit}' \
    >"$transport_file"
}

correction_message() {
  local reason=$1
  local remaining=$2
  jq -cn \
    --arg version "$PROTOCOL_VERSION" \
    --arg reason "$reason" \
    --argjson remaining "$remaining" \
    '{protocol_version:$version,type:"protocol_correction",ok:false,error_code:$reason,retries_remaining:$remaining,required:"Return exactly one valid runstead_action or runstead_final envelope; never claim local execution without an envelope."}'
}

run_session() {
  local session_number=$1
  local session_name session_dir fixture_file seen_actions
  local messages request_count successful_tool_turns correction_used correction_pending repeated_actions
  local request_file response_file headers_file curl_error_file model_content_file parser_tmp parser_saved observation_file
  local request_json model_content kind executable reason tool fingerprint result observation correction
  local completed=false failure_reason=

  session_name=$(printf 'session-%02d' "$session_number")
  session_dir="$OUTPUT_DIR/sessions/$session_name"
  mkdir -p "$session_dir"
  chmod 700 "$session_dir"
  seen_actions=$(mktemp "${TMPDIR:-/tmp}/runstead-seen-actions.XXXXXX")
  messages=$(jq -cn \
    --arg system 'You are a protocol test subject. Runstead owns the action protocol and executes the simulated read-only tools. Never use native tool calls. On each turn return exactly one tagged envelope: either <runstead_action> containing one strict JSON object with version "runstead.protocol.v1", tool and arguments, or <runstead_final> containing version, status, summary and evidence, where every evidence entry is a typed citation object {"evidence_id":"<obs id>","tool":"<tool>"} naming the tool that produced the observation. Do not claim to have read or listed anything yourself. Available tools: read_file(path), list_files(path). A valid action may be surrounded by short prose; the tagged envelope remains mandatory.' \
    --arg task 'Inspect the fixture workspace through at least five successful read-only tool turns. Use both read_file and list_files, and finish only after the observations support a concise evidence-based summary.' \
    '[{role:"system",content:$system},{role:"user",content:$task}]')
  request_count=0
  successful_tool_turns=0
  correction_used=0
  correction_pending=false
  repeated_actions=0

  if [[ $MODE == offline ]]; then
    fixture_file="$FIXTURES_DIR/sessions/$session_name.jsonl"
    [[ -f $fixture_file ]] || { failure_reason=missing_fixture; }
  fi
  if [[ -n $failure_reason ]]; then
    rm -f "$seen_actions"
    write_event "$(jq -cn --arg session "$session_name" --arg reason "$failure_reason" '{event:"session",session:$session,status:"failed",reason:$reason,successful_tool_turns:0,requests:0,corrections:0,repeats:0}')"
    return 0
  fi

  while (( request_count < TOOL_TURNS + CORRECTION_LIMIT + 5 )); do
    request_count=$((request_count + 1))
    CURRENT_SESSION_ID="runstead-$RUN_ID-$session_name"
    CURRENT_REQUEST=$request_count
    request_file=$(mktemp "${TMPDIR:-/tmp}/runstead-request.XXXXXX")
    response_file=$(mktemp "${TMPDIR:-/tmp}/runstead-response.XXXXXX")
    headers_file=$(mktemp "${TMPDIR:-/tmp}/runstead-headers.XXXXXX")
    curl_error_file=$(mktemp "${TMPDIR:-/tmp}/runstead-curl-error.XXXXXX")
    model_content_file=$(mktemp "${TMPDIR:-/tmp}/runstead-model.XXXXXX")
    request_json=$(request_body "$messages")
    printf '%s\n' "$request_json" >"$request_file"

    HTTP_STATUS=200
    CURL_EXIT=0
    TRANSPORT_CLASS=success
    if [[ $MODE == live ]]; then
      if ! invoke_live "$request_file" "$response_file" "$headers_file" "$curl_error_file"; then
        write_captures "$session_dir" "$request_count" "$request_file" "$response_file" "$headers_file" "$curl_error_file"
        write_event "$(jq -cn --arg session "$session_name" --argjson turn "$request_count" --arg class "$TRANSPORT_CLASS" --arg status "$HTTP_STATUS" '{event:"model_response",session:$session,turn:$turn,transport_class:$class,http_status:$status,parse:null}')"
        failure_reason=$TRANSPORT_CLASS
        rm -f "$request_file" "$response_file" "$headers_file" "$curl_error_file" "$model_content_file"
        break
      fi
    else
      jq -er --argjson turn "$request_count" 'select(.turn == $turn) | .response' "$fixture_file" >"$response_file" || {
        failure_reason=missing_replay_turn
        rm -f "$request_file" "$response_file" "$headers_file" "$curl_error_file" "$model_content_file"
        break
      }
      printf 'fixture replay\n' >"$headers_file"
      : >"$curl_error_file"
    fi

    write_captures "$session_dir" "$request_count" "$request_file" "$response_file" "$headers_file" "$curl_error_file"
    if [[ $MODE == live ]]; then
      if ! normalize_provider_response "$response_file" "$model_content_file"; then
        write_event "$(jq -cn --arg session "$session_name" --argjson turn "$request_count" --arg class "$TRANSPORT_CLASS" --arg provider "$PROVIDER_CLASS" --arg status "$HTTP_STATUS" '{event:"model_response",session:$session,turn:$turn,transport_class:$class,provider_class:$provider,http_status:$status,parse:null}')"
        failure_reason=$PROVIDER_CLASS
        rm -f "$request_file" "$response_file" "$headers_file" "$curl_error_file" "$model_content_file"
        break
      fi
    else
      cp "$response_file" "$model_content_file"
      PROVIDER_CLASS=success
    fi

    model_content=$(<"$model_content_file")
    sanitize_file "$model_content_file" "$session_dir/turn-$(printf '%02d' "$request_count").model.txt"
    parser_tmp=$(mktemp "${TMPDIR:-/tmp}/runstead-parser.XXXXXX")
    "$PARSE_SCRIPT" "$model_content_file" >"$parser_tmp"
    parser_saved="$session_dir/turn-$(printf '%02d' "$request_count").parser.json"
    sanitize_file "$parser_tmp" "$parser_saved"
    kind=$(jq -r '.kind' "$parser_saved")
    executable=$(jq -r '.executable' "$parser_saved")
    reason=$(jq -r '.reason' "$parser_saved")
    tool=$(jq -r '.tool // empty' "$parser_saved")
    write_event "$(jq -cn --arg session "$session_name" --argjson turn "$request_count" --arg class "$TRANSPORT_CLASS" --arg provider "$PROVIDER_CLASS" --arg status "$HTTP_STATUS" --slurpfile parse "$parser_saved" '{event:"model_response",session:$session,turn:$turn,transport_class:$class,provider_class:$provider,http_status:$status,parse:$parse[0]}')"

    messages=$(append_message "$messages" assistant "$model_content")
    if [[ $kind == final && $executable == true && $(jq -r '.status' "$parser_saved") == complete && $successful_tool_turns -ge $TOOL_TURNS ]]; then
      if [[ $correction_pending == true ]]; then
        write_event "$(jq -cn --arg session "$session_name" --argjson turn "$request_count" '{event:"correction_success",session:$session,turn:$turn}')"
        correction_pending=false
      fi
      completed=true
      rm -f "$request_file" "$response_file" "$headers_file" "$curl_error_file" "$model_content_file" "$parser_tmp"
      break
    fi

    if [[ $kind == action && $executable == true ]]; then
      fingerprint=$(jq -cS '{tool,arguments}' "$parser_saved")
      if grep -Fqx "$fingerprint" "$seen_actions"; then
        repeated_actions=$((repeated_actions + 1))
        write_event "$(jq -cn --arg session "$session_name" --argjson turn "$request_count" --arg tool "$tool" '{event:"repeat_detected",session:$session,turn:$turn,tool:$tool}')"
        reason=repeated_action
      else
        printf '%s\n' "$fingerprint" >>"$seen_actions"
        observation_file="$session_dir/turn-$(printf '%02d' "$request_count").observation.json"
        result=$(execute_tool "$parser_saved")
        if [[ $(jq -r '.ok' <<<"$result") == true ]]; then
          successful_tool_turns=$((successful_tool_turns + 1))
        fi
        observation=$(jq -cn \
          --arg version "$PROTOCOL_VERSION" \
          --arg tool "$tool" \
          --argjson arguments "$(jq -c '.arguments' "$parser_saved")" \
          --argjson result "$result" \
          '{protocol_version:$version,type:"tool_observation",tool:$tool,arguments:$arguments,ok:$result.ok,result:$result}')
        printf '%s\n' "$observation" >"$observation_file"
        write_event "$(jq -cn --arg session "$session_name" --argjson turn "$request_count" --arg tool "$tool" --argjson ok "$(jq -r '.ok' <<<"$result")" '{event:"tool_execution",session:$session,turn:$turn,tool:$tool,ok:$ok}')"
        messages=$(append_message "$messages" user "$observation")
        if [[ $correction_pending == true ]]; then
          write_event "$(jq -cn --arg session "$session_name" --argjson turn "$request_count" '{event:"correction_success",session:$session,turn:$turn}')"
          correction_pending=false
        fi
        rm -f "$request_file" "$response_file" "$headers_file" "$curl_error_file" "$model_content_file" "$parser_tmp"
        continue
      fi
    elif [[ $kind == final && $executable == true ]]; then
      reason=premature_final
    fi

    if (( correction_used < CORRECTION_LIMIT )); then
      correction_used=$((correction_used + 1))
      correction_pending=true
      correction=$(correction_message "$reason" "$((CORRECTION_LIMIT - correction_used))")
      messages=$(append_message "$messages" user "$correction")
      write_event "$(jq -cn --arg session "$session_name" --argjson turn "$request_count" --arg reason "$reason" --argjson retry "$correction_used" --argjson remaining "$((CORRECTION_LIMIT - correction_used))" '{event:"correction",session:$session,turn:$turn,reason:$reason,retry:$retry,retries_remaining:$remaining}')"
    else
      failure_reason=correction_limit_exhausted
      rm -f "$request_file" "$response_file" "$headers_file" "$curl_error_file" "$model_content_file" "$parser_tmp"
      break
    fi
    rm -f "$request_file" "$response_file" "$headers_file" "$curl_error_file" "$model_content_file" "$parser_tmp"
  done

  rm -f "$seen_actions"
  if [[ $completed == true ]]; then
    write_event "$(jq -cn --arg session "$session_name" --argjson successful_tool_turns "$successful_tool_turns" --argjson requests "$request_count" --argjson corrections "$correction_used" --argjson repeats "$repeated_actions" '{event:"session",session:$session,status:"completed",successful_tool_turns:$successful_tool_turns,requests:$requests,corrections:$corrections,repeats:$repeats}')"
  else
    [[ -n $failure_reason ]] || failure_reason=max_requests_exhausted
    write_event "$(jq -cn --arg session "$session_name" --arg reason "$failure_reason" --argjson successful_tool_turns "$successful_tool_turns" --argjson requests "$request_count" --argjson corrections "$correction_used" --argjson repeats "$repeated_actions" '{event:"session",session:$session,status:"failed",reason:$reason,successful_tool_turns:$successful_tool_turns,requests:$requests,corrections:$corrections,repeats:$repeats}')"
  fi
}

generate_report() {
  jq -s \
    --arg schema "runstead.protocol.report.v1" \
    --arg run_id "$RUN_ID" \
    --arg mode "$MODE" \
    --argjson configured_sessions "$SESSIONS" \
    --argjson configured_tool_turns "$TOOL_TURNS" \
    --argjson correction_limit "$CORRECTION_LIMIT" \
    '
      def n(f): map(select(f)) | length;
      def required_sessions: n(.event == "session" and .status == "completed" and (.successful_tool_turns // 0) >= $configured_tool_turns);
      {
        schema:$schema,
        run_id:$run_id,
        mode:$mode,
        sessions:{
          configured:$configured_sessions,
          completed:n(.event == "session" and .status == "completed"),
          failed:n(.event == "session" and .status == "failed"),
          completed_with_required_tool_turns:required_sessions
        },
        transport:{
          attempts:n(.event == "model_response"),
          successes:n(.event == "model_response" and .transport_class == "success"),
          failures:n(.event == "model_response" and .transport_class != "success"),
          transport_errors:n(.event == "model_response" and .transport_class == "transport_error"),
          auth_failures:n(.event == "model_response" and .transport_class == "auth_failure"),
          provider_errors:n(.event == "model_response" and ((.transport_class == "provider_http_error") or (.provider_class // "" | startswith("provider_"))))
        },
        protocol:{
          corpus_cases:n(.event == "corpus_case"),
          corpus_schema_valid:n(.event == "corpus_case" and .parse.schema_valid == true),
          parser_schema_valid:n(.event == "model_response" and .parse.schema_valid == true),
          tool_turns_completed:n(.event == "tool_execution" and .ok == true),
          tool_turns_failed:n(.event == "tool_execution" and .ok == false),
          action_attempts:n(.event == "model_response" and .parse.kind == "action"),
          malformed_actions:(n(.event == "corpus_case" and .parse.reason == "malformed_json") + n(.event == "model_response" and .parse.reason == "malformed_json")),
          unknown_tools:(n(.event == "corpus_case" and .parse.reason == "unknown_tool") + n(.event == "model_response" and .parse.reason == "unknown_tool")),
          protocol_refusals:(n(.event == "corpus_case" and .parse.reason == "protocol_refusal") + n(.event == "model_response" and .parse.reason == "protocol_refusal")),
          unsupported_execution_claims:(n(.event == "corpus_case" and .parse.reason == "unsupported_execution_claim") + n(.event == "model_response" and .parse.reason == "unsupported_execution_claim")),
          mixed_prose_actions:(n(.event == "corpus_case" and .parse.mixed_prose == true) + n(.event == "model_response" and .parse.mixed_prose == true)),
          correction_limit:$correction_limit,
          correction_attempts:n(.event == "correction"),
          correction_successes:n(.event == "correction_success"),
          repeated_actions:n(.event == "repeat_detected")
        },
        failure_classification:{
          transport:n(.event == "model_response" and .transport_class == "transport_error"),
          provider:n(.event == "model_response" and ((.transport_class == "provider_http_error") or (.transport_class == "auth_failure") or ((.provider_class // "") | startswith("provider_")))),
          protocol:n(.event == "model_response" and .transport_class == "success" and .parse != null and .parse.executable != true),
          policy:n(.event == "repeat_detected")
        },
        decision:{
          status:(if $mode == "live" and (n(.event == "session" and .status == "failed") == 0) and required_sessions == $configured_sessions then "adopt" else "revise" end),
          reason:(if $mode == "live" and (n(.event == "session" and .status == "failed") == 0) and required_sessions == $configured_sessions then "Live criteria passed; the candidate is ready for M1 implementation." else "Offline fixtures prove the parser and bounded harness; run --live with three independent sessions before adopting the protocol for M1." end)
        }
      }
    ' "$EVENTS_FILE" >"$OUTPUT_DIR/report.json"

  {
    printf '# Runstead protocol experiment report\n\n'
    printf '%s\n' "- Mode: $MODE"
    printf '%s\n' "- Run ID: $RUN_ID"
    printf '%s\n\n' "- Output: $OUTPUT_DIR"
    jq -r '
      "## Evidence\n\n" +
      "- Sessions: " + (.sessions.completed|tostring) + "/" + (.sessions.configured|tostring) + " completed\n" +
      "- Sessions meeting the per-session turn gate: " + (.sessions.completed_with_required_tool_turns|tostring) + "/" + (.sessions.configured|tostring) + "\n" +
      "- Successful read-only tool turns: " + (.protocol.tool_turns_completed|tostring) + "\n" +
      "- Correction attempts/successes: " + (.protocol.correction_attempts|tostring) + "/" + (.protocol.correction_successes|tostring) + " (limit " + (.protocol.correction_limit|tostring) + ")\n" +
      "- Repeated actions: " + (.protocol.repeated_actions|tostring) + "\n" +
      "- Corpus cases: " + (.protocol.corpus_cases|tostring) + "\n" +
      "- Transport failures: " + (.transport.failures|tostring) + "\n\n" +
      "## Failure classification\n\n" +
      "- Transport errors: " + (.failure_classification.transport|tostring) + "\n" +
      "- Provider failures: " + (.failure_classification.provider|tostring) + "\n" +
      "- Protocol: " + (.failure_classification.protocol|tostring) + "\n" +
      "- Policy/repetition: " + (.failure_classification.policy|tostring) + "\n\n" +
      "## Decision\n\n" + (.decision.status | ascii_upcase) + ": " + .decision.reason + "\n"
    ' "$OUTPUT_DIR/report.json"
  } >"$OUTPUT_DIR/report.md"
}

WORKSPACE_REAL=$(CDPATH= cd "$FIXTURES_DIR/workspace" && pwd -P)
corpus_cases
for ((session=1; session<=SESSIONS; session++)); do
  run_session "$session"
done
generate_report

printf 'Report: %s\n' "$OUTPUT_DIR/report.md"
if jq -e '.sessions.failed == 0' "$OUTPUT_DIR/report.json" >/dev/null; then
  exit 0
fi
exit 1
