#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"

GO_BIN="${GO_BIN:-$(command -v go || true)}"
if [[ -z "$GO_BIN" || ! -x "$GO_BIN" ]]; then
  echo "GO_BIN must point to a Go 1.26+ executable (current: ${GO_BIN:-unset})" >&2
  exit 2
fi
GO_VERSION="$($GO_BIN version 2>/dev/null || true)"
if [[ ! "$GO_VERSION" =~ go([0-9]+)\.([0-9]+) ]]; then
  echo "unable to parse Go version from: ${GO_VERSION:-unknown}" >&2
  exit 2
fi
GO_MAJOR="${BASH_REMATCH[1]}"
GO_MINOR="${BASH_REMATCH[2]}"
if (( GO_MAJOR < 1 || (GO_MAJOR == 1 && GO_MINOR < 26) )); then
  echo "Go 1.26+ is required; found: $GO_VERSION" >&2
  exit 2
fi
CHROMIUM_PATH="${CHROMIUM_PATH:-$(command -v chromium || command -v chromium-browser || true)}"
if [[ -z "$CHROMIUM_PATH" || ! -x "$CHROMIUM_PATH" ]]; then
  echo "CHROMIUM_PATH must point to an installed Chromium executable" >&2
  exit 2
fi
FIXTURE_ADDR="${RUNSTEAD_FIXTURE_ADDR:-127.0.0.1:18765}"
FIXTURE_URL="${RUNSTEAD_FIXTURE_URL:-http://${FIXTURE_ADDR}}"
START_FIXTURE="${START_FIXTURE:-1}"

mkdir -p output profiles
FIXTURE_PID=""
cleanup() {
  if [[ -n "$FIXTURE_PID" ]]; then
    kill "$FIXTURE_PID" 2>/dev/null || true
    wait "$FIXTURE_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

if [[ "$START_FIXTURE" == "1" ]]; then
  (cd fixture && "$GO_BIN" build -o ../fixture-server .)
  RUNSTEAD_FIXTURE_ADDR="$FIXTURE_ADDR" "$ROOT/fixture-server" > output/fixture.log 2>&1 &
  FIXTURE_PID=$!
  for attempt in 1 2 3 4 5 6 7 8 9 10; do
    if curl -fsS "${FIXTURE_URL}/healthz" >/dev/null; then break; fi
    sleep 0.2
  done
fi
curl -fsS "${FIXTURE_URL}/healthz" >/dev/null

(cd playwright && PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1 npm ci --ignore-scripts && RUNSTEAD_FIXTURE_URL="$FIXTURE_URL" CHROMIUM_PATH="$CHROMIUM_PATH" npm test && RUNSTEAD_FIXTURE_URL="$FIXTURE_URL" CHROMIUM_PATH="$CHROMIUM_PATH" node benchmark.mjs)

(
  cd chromedp
  GOTOOLCHAIN=local "$GO_BIN" test ./...
  GOTOOLCHAIN=local "$GO_BIN" vet ./...
  GOTOOLCHAIN=local "$GO_BIN" build -o ../chromedp-runner .
  cd ..
  RUNSTEAD_FIXTURE_URL="$FIXTURE_URL" CHROMIUM_PATH="$CHROMIUM_PATH" RUNSTEAD_OUTPUT_DIR=output RUNSTEAD_PROFILE_ROOT=profiles/chromedp ./chromedp-runner
)

echo "Synthetic substrate bake-off completed; see output/*.json"
