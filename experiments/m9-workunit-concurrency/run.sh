#!/usr/bin/env bash
# M9 evidence-gate benchmark runner (issue #53).
#
# Executes the wall-clock reproduction harness in
# cmd/runstead/workunit_m9_bench_test.go against the REAL composition root
# (driver + SQLite + governor executor + agent loops + tools) and captures the
# M9CELL evidence lines into a dated results file under this directory. The
# results are environment-dependent by design; they feed the versioned report
# in docs/m9-workunit-concurrency-evidence.md and are NEVER asserted by CI.
#
# Usage:
#   bash experiments/m9-workunit-concurrency/run.sh [--benchtime 20x] [--race]
#   bash experiments/m9-workunit-concurrency/run.sh --quick   # 5x, sanity only
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "$here/../.." && pwd)"

benchtime="20x"
race=""
for arg in "$@"; do
  case "$arg" in
    --benchtime=*) benchtime="${arg#--benchtime=}" ;;
    --quick) benchtime="5x" ;;
    --race) race="-race" ;;
    *)
      echo "unknown argument: $arg" >&2
      exit 2
      ;;
  esac
done

run_at="$(date -u +%Y-%m-%dT%H%M%SZ)"
results_dir="$here/results"
mkdir -p "$results_dir"
out="$results_dir/m9-$run_at.txt"
err="$results_dir/m9-$run_at.err"

echo "M9 workunit concurrency evidence harness"
echo "  repo:      $repo"
echo "  benchtime: $benchtime"
echo "  race:      ${race:-off}"
echo "  output:    $out"

set +e
(cd "$repo" && go test $race ./cmd/runstead \
  -run '^$' \
  -bench '^BenchmarkM9' \
  -benchtime="$benchtime" \
  -count=1 \
  -v \
  >"$out" 2>"$err")
status=$?
set -e

if [ "$status" -ne 0 ]; then
  echo "FAILED (exit $status); stderr tail:" >&2
  tail -30 "$err" >&2
  exit "$status"
fi

echo "captured; cell summary:"
grep -E '^    .*M9CELL ' "$out" | sed 's/^    //' || true
echo
echo "full log: $out"