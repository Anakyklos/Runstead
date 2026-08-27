#!/usr/bin/env bash
# Runstead compatible-provider live smoke (issue #14, Part 7).
#
# OPT-IN ONLY: normal CI executes zero live traffic. This script refuses to
# run unless RUNSTEAD_LIVE_SMOKE=1 is explicitly set by the operator.
#
# Prohibited: quota probing, concurrency escalation, key/account/model
# rotation, fallback, rate-limit workarounds, fabricated success. If a family
# cannot be exercised with the operator's available endpoint/credentials, do
# NOT run this smoke; record the family as operationally unproven.
#
# A failing live attempt (auth, rate limit, timeout, 5xx) is EVIDENCE, not a
# reason to die silently: the script always captures the exit code and still
# produces the sanitized record/inspection output.
#
# The smoke records, from the real durable task state (runstead inspect):
#   provider ID, protocol family, exact model, sanitized config identity,
#   adapter version, governor admission/attempts and delivery outcomes.
# The record is written to --output DIR as a sanitized text file.
set -Eeuo pipefail

if [[ "${RUNSTEAD_LIVE_SMOKE:-0}" != "1" ]]; then
  echo "live smoke is opt-in: set RUNSTEAD_LIVE_SMOKE=1 to run real endpoint traffic" >&2
  exit 2
fi

BIN=${RUNSTEAD_BIN:-$(CDPATH= cd "$(dirname "$0")/../.." && pwd)/runstead}
OUTPUT=
PROVIDERS=
PROVIDER_ID=
TASK="live smoke check"
WORKSPACE=
STATE_DIR=
ACCEPTANCE=
RECIPES=
RECIPE_POLICY=
WRITE_POLICY=
LOG_LEVEL=info
MIN_START_INTERVAL=

usage() {
  cat <<'EOF'
Usage: run.sh --providers FILE --provider-id ID --workspace PATH [options]

Required:
  --providers FILE     provider declarations file (RUNSTEAD_PROVIDERS)
  --provider-id ID     exactly one configured provider_id (RUNSTEAD_PROVIDER_ID)
  --workspace PATH     workspace path (RUNSTEAD_WORKSPACE)

Options:
  --task PROMPT        task prompt (default: "live smoke check")
  --acceptance FILE    operator acceptance plan (required to reach completed)
  --recipes FILE       operator recipe catalog (optional)
  --recipe-policy SPEC recipe policy modes (optional)
  --write-policy SPEC  write policy modes (optional)
  --state-dir DIR      durable state directory (default: temp dir under OUTPUT)
  --output DIR         retain the sanitized live record in DIR (required)
  --log-level LEVEL    debug|info|warn|error (default info)
  --min-start-interval DURATION  account governor pacing override (default 5s)

Prohibited: quota probing, concurrency escalation, rotation, fallback.
EOF
}

while (($#)); do
  case $1 in
    --providers) PROVIDERS=$2; shift 2 ;;
    --provider-id) PROVIDER_ID=$2; shift 2 ;;
    --workspace) WORKSPACE=$2; shift 2 ;;
    --task) TASK=$2; shift 2 ;;
    --acceptance) ACCEPTANCE=$2; shift 2 ;;
    --recipes) RECIPES=$2; shift 2 ;;
    --recipe-policy) RECIPE_POLICY=$2; shift 2 ;;
    --write-policy) WRITE_POLICY=$2; shift 2 ;;
    --state-dir) STATE_DIR=$2; shift 2 ;;
    --output) OUTPUT=$2; shift 2 ;;
    --log-level) LOG_LEVEL=$2; shift 2 ;;
    --min-start-interval) MIN_START_INTERVAL=$2; shift 2 ;;
    --help|-h) usage; exit 0 ;;
    *) echo "unknown option: $1" >&2; usage; exit 2 ;;
  esac
done

if [[ -z "$PROVIDERS" || -z "$PROVIDER_ID" || -z "$WORKSPACE" || -z "$OUTPUT" ]]; then
  echo "missing required options" >&2
  usage
  exit 2
fi
mkdir -p "$OUTPUT"
STATE_DIR=${STATE_DIR:-$(mktemp -d "$OUTPUT/state.XXXXXX")}

run_args=(run --task "$TASK" --workspace "$WORKSPACE" --providers "$PROVIDERS" --provider-id "$PROVIDER_ID" --state-dir "$STATE_DIR" --log-level "$LOG_LEVEL")
[[ -n "$ACCEPTANCE" ]] && run_args+=(--acceptance "$ACCEPTANCE")
[[ -n "$RECIPES" ]] && run_args+=(--recipes "$RECIPES" --recipe-policy "${RECIPE_POLICY:-approval_required}")
[[ -n "$WRITE_POLICY" ]] && run_args+=(--write-policy "$WRITE_POLICY")
[[ -n "$MIN_START_INTERVAL" ]] && run_args+=(--min-start-interval "$MIN_START_INTERVAL")

echo "live smoke: provider=$PROVIDER_ID task=$(printf %q "$TASK")"

# Capture the real exit code without dying on a live failure: a non-zero live
# attempt is evidence that MUST still produce the sanitized record.
set +e
"$BIN" "${run_args[@]}" >"$OUTPUT/result.stdout.log" 2>"$OUTPUT/trace.stderr.log"
code=$?
set -e
cat "$OUTPUT/result.stdout.log"
echo "live smoke exit=$code" | tee -a "$OUTPUT/record.txt"

# field extracts one sanitized identity/delivery field from the durable
# inspection; missing values stay empty (never guessed, never fabricated).
field() {
  grep -o "$1" "$OUTPUT/inspect.txt" 2>/dev/null | head -1 | cut -d= -f2- || true
}

task_id=$(sed -n 's/^task: //p' "$OUTPUT/trace.stderr.log" | head -1)
if [[ -n "$task_id" ]]; then
  "$BIN" inspect "$task_id" --state-dir "$STATE_DIR" > "$OUTPUT/inspect.txt" 2>&1 || \
    { echo "inspect unavailable for task $task_id" >>"$OUTPUT/record.txt"; }
  {
    echo "provider_id: $PROVIDER_ID"
    echo "protocol_family: $(field 'protocol_family=[^ ]*')"
    echo "model: $(field 'model=[^ ]*')"
    echo "config_identity: $(field 'config_identity=[^ ]*')"
    echo "adapter_version: $(field 'adapter_version=[^ ]*')"
    echo "task: $task_id"
    echo "outcome: $(grep -o 'outcome=[^ ]*' "$OUTPUT/result.stdout.log" | head -1 | cut -d= -f2- || true)"
  } | tee -a "$OUTPUT/record.txt"
  echo "sanitized live record: $OUTPUT/record.txt" >&2
else
  echo "no task id captured; run failed before task creation" | tee -a "$OUTPUT/record.txt" >&2
fi
exit "$code"
