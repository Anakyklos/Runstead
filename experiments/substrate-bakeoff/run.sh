#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"

mkdir -p output profiles

(cd fixture && go build -o ../fixture-server .)
"$ROOT/fixture-server" > output/fixture.log 2>&1 &
FIXTURE_PID=$!
cleanup() {
  kill "$FIXTURE_PID" 2>/dev/null || true
  wait "$FIXTURE_PID" 2>/dev/null || true
}
trap cleanup EXIT

for attempt in 1 2 3 4 5; do
  if curl -fsS http://127.0.0.1:18765/healthz >/dev/null; then break; fi
  sleep 0.2
done
curl -fsS http://127.0.0.1:18765/healthz >/dev/null

(cd playwright && PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1 npm install --ignore-scripts && RUNSTEAD_FIXTURE_URL=http://127.0.0.1:18765 npm test && RUNSTEAD_FIXTURE_URL=http://127.0.0.1:18765 node benchmark.mjs)

(
  cd chromedp
  GOTOOLCHAIN=local /home/ubuntu/go1.26.1/bin/go mod tidy
  GOTOOLCHAIN=local /home/ubuntu/go1.26.1/bin/go test ./...
  GOTOOLCHAIN=local /home/ubuntu/go1.26.1/bin/go vet ./...
  GOTOOLCHAIN=local /home/ubuntu/go1.26.1/bin/go build -o ../chromedp-runner .
  cd ..
  RUNSTEAD_OUTPUT_DIR=output RUNSTEAD_PROFILE_ROOT=profiles/chromedp ./chromedp-runner
)

echo "Synthetic substrate bake-off completed; see output/*.json"
