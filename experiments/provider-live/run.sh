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
  --state-dir DIR      durable state directory (default: temp dir)
  --output DIR         retain the sanitized live record in DIR (required)
  --log-level LEVEL    debug|info|warn|error (default info)
  --min-start-interval DURATION  governor pacing (default 5s)

Prohibited: quota probing, concurrency escalation, rotation, fallback.
EOF
  exit 0
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
    --help|-h) usage ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done

[[ -n "$PROVIDERS" && -n "$PROVIDER_ID" && -n "$WORKSPACE" && -n "$OUTPUT" ]] || { echo "missing required options" >&2; usage; exit 2; }
mkdir -p "$OUTPUT"
STATE_DIR=${STATE_DIR:-$(mktemp -d "$OUTPUT/state.XXXXXX")}

run_args=(run --task "$TASK" --workspace "$WORKSPACE" --providers "$PROVIDERS" --provider-id "$PROVIDER_ID" --state-dir "$STATE_DIR" --log-level "$LOG_LEVEL")
[[ -n "$ACCEPTANCE" ]] && run_args+=(--acceptance "$ACCEPTANCE")
[[ -n "$RECIPES" ]] && run_args+=(--recipes "$RECIPES" --recipe-policy "${RECIPE_POLICY:-approval_required}")
[[ -n "$WRITE_POLICY" ]] && run_args+=(--write-policy "$WRITE_POLICY")
[[ -n "$MIN_START_INTERVAL" ]] && run_args+=(--min-start-interval "$MIN_START_INTERVAL")

echo "live smoke: provider=$PROVIDER_ID task=$(printf %q "$TASK")"
"$BIN" "${run_args[@]}" 2>"$OUTPUT/trace.stderr.log" | tee "$OUTPUT/result.stdout.log"
code=${PIPESTATUS[0]}
echo "live smoke exit=$code" | tee -a "$OUTPUT/record.txt"

# The sanitized live record derives from the real durable task state: provider
# identity, family, model, sanitized config identity, adapter version,
# governor admission and delivery outcomes (runstead inspect).
task_id=$(sed -n 's/^task: //p' "$OUTPUT/trace.stderr.log" | head -1)
if [[ -n "$task_id" ]]; then
  "$BIN" inspect "$task_id" --state-dir "$STATE_DIR" > "$OUTPUT/inspect.txt" 2>&1 || true
  {
    echo "provider_id: $PROVIDER_ID"
    echo "protocol_family: $(grep -o 'protocol_family=[^ ]*' "$OUTPUT/inspect.txt" | head -1 | cut -d= -f2-)"
    echo "model: $(grep -o 'model=[^ ]*' "$OUTPUT/inspect.txt" | head -1 | cut -d= -f2-)"
    echo "config_identity: $(grep -o 'config_identity=[^ ]*' "$OUTPUT/inspect.txt" | head -1 | cut -d= -f2-)"
    echo "adapter_version: $(grep -o 'adapter_version=[^ ]*' "$OUTPUT/inspect.txt" | head -1 | cut -d= -f2-)"
    echo "task: $task_id"
    echo "outcome: $(grep -o 'outcome=[^ ]*' "$OUTPUT/result.stdout.log" | head -1 | cut -d= -f2-)"
  } | tee -a "$OUTPUT/record.txt"
  echo "sanitized live record: $OUTPUT/record.txt" >&2
else
  echo "no task id captured; run failed before task creation" | tee -a "$OUTPUT/record.txt" >&2
fi
exit "$code"
