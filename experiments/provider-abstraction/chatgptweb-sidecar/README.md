# ChatGPT Web Sidecar — Python + nodriver (Research Prototype)

Standalone experiment for Issue #16 provider abstraction.

**Status**: Research prototype — NOT production ready. Core blockers from review #4962396582 tracked in PR #77.

## Architecture

```
Runstead Core (Go)                    ChatGPT Web Sidecar (Python)
+------------------+                 Python 3.11+ + nodriver
| Agent Loop       |  JSON-RPC       * Dedicated browser profile per account (user_data_dir)
| Governor         |  over stdio     * No cookie extraction; all HTTP via browser CDP fetch
| State (SQLite)   |  <----------->  * SSE streaming + echo suppression reconciler
+------------------+                 * Sentinel handshake (nodriver browser, detects challenges only)
                                     * Drift detection (sentinel/sdk.js hash probe)
                                     * No cookie persistence; browser profile owns credentials
                                     * JSON-RPC 2.0 over stdio
```

## Key Differences from Previous Versions

| Previous (removed) | Current (prototype) |
|-------------------|---------------------|
| Cookie persistence (encrypted) | Browser profile owns credentials; no cookie export |
| Access-token reuse via `/api/auth/session` | CDP fetch to check auth state without materializing cookies |
| Sentinel/Turnstile auto-solving | Challenge DETECTION only; human must resolve |
| Local circuit breaker | Fail-closed transport evidence (no auto-retry) |
| `cookies.enc` files | None — browser profile is source of truth |

## Quick Start

```bash
cd experiments/provider-abstraction/chatgptweb-sidecar

# Create venv
python3 -m venv .venv
source .venv/bin/activate

# Install dependencies
pip install -e .

# Run sidecar (stdio JSON-RPC)
python -m chatgptweb
```

## Configuration

Environment variables:
- `CHATGPTWEB_ACCOUNTS_DIR` — directory for browser profiles (default: `~/.local/share/runstead/chatgptweb`)
- `CHATGPTWEB_MASTER_KEY` — master key for local file encryption (required)
- `CHATGPTWEB_PROXY` — optional proxy (e.g., `http://user:pass@host:port`)
- `CHATGPTWEB_DEFAULT_ACCOUNT` — account ID to use by default
- `CHATGPTWEB_HEADLESS` — browser headless mode (default: `false` for better bypass)

## JSON-RPC Protocol (stdio)

### Request (Go → Python)
```json
{
  "jsonrpc": "2.0",
  "id": "req-001",
  "method": "complete",
  "params": {
    "client_request_id": "cli-123-456",
    "model": "gpt-5.6-luna",
    "messages": [
      {"role": "system", "content": "You are a coding assistant."},
      {"role": "user", "content": "Read README.md"}
    ],
    "stream": true
  }
}
```

### Response (Python → Go) — Streaming
```json
{"jsonrpc": "2.0", "method": "stream_delta", "params": {"delta": "Hello", "done": false}}
{"jsonrpc": "2.0", "method": "stream_delta", "params": {"delta": " world", "done": true}}
```

### Final Response
```json
{"jsonrpc": "2.0", "id": "req-001", "result": {
  "content": "Hello world",
  "metadata": {
    "client_request_id": "cli-123-456",
    "status_code": 200,
    "request_id": null,
    "session_id": null,
    "duration_ms": 12500,
    "model": "gpt-5.6-luna",
    "transport_state": "completed",
    "send_count": 1,
    "retry_after": null,
    "reset_at": null,
    "challenge_type": null
  }
}}
```

### Error Response (with transport evidence)
```json
{"jsonrpc": "2.0", "id": "req-001", "error": {
  "code": -32001,
  "message": "Authentication failed: HTTP 401",
  "data": {
    "challenge_type": "login_wall",
    "reason": "Authentication failed: HTTP 401",
    "evidence": {
      "state": "transport_failed",
      "send_count": 1,
      "http_status": 401,
      "error_code": "authentication_required",
      "duration_ms": 500,
      "challenge_type": "login_wall"
    }
  }
}}
```

## Methods

| Method | Params | Description |
|--------|--------|-------------|
| `initialize` | `config` | Called once at startup |
| `complete` | `client_request_id`, `model`, `messages`, `stream` | Execute completion |
| `health_check` | — | Validate auth, model, endpoint |
| `models` | — | List available models |
| `warm_session` | `account_id?` | Force session warm (raises challenge if needed) |

## Session Warming

1. Browser starts with dedicated `--user-data-dir` per account
2. Navigate to `https://chatgpt.com`
3. `health_check` → CDP fetch to `/api/auth/session` (no cookie extraction)
4. If authenticated → session warm, ready for `complete`
5. If challenge detected (Turnstile, CAPTCHA, login wall) → raises `SessionNotReady` with challenge type
6. Human resolves challenge interactively
7. `wait_for_human()` polls until challenge resolved

**NO AUTO-SOLVE** — challenges are detected and classified only; human must resolve.

## SSE Reconciliation (Echo Suppression)

ChatGPT Web sends **cumulative deltas** with echo suppression:
- `SSEReconciler` tracks `last_content`, emits only NEW deltas
- 7 deterministic tests pass (first chunk, cumulative, duplicate, growing, malformed, `[DONE]`, no negative slicing)

## Drift Detection

Periodic probe of `https://chatgpt.com/backend-api/sentinel/sdk.js`:
- Hash response body (SHA256)
- Compare to known-good baseline hash (persisted per account)
- On drift → `DriftDetected` exception blocks model-effect send
- Fail-closed: probe failure = potential drift

## Transport Evidence

`TransportEvidence` with 8 states, `send_count`, typed errors:
- `NO_SEND_OBSERVED` → `SEND_OBSERVED` (headers received) → `RESPONSE_STARTED` → `COMPLETED`
- Errors: `TRANSPORT_FAILED`, `TIMEOUT_UNCERTAIN`, `CANCELED`
- No `DeliveryState` — evidence is NOT derived from `err == nil` or HTTP 200
- Error paths yield evidence as JSON-serializable dicts

## Tests

```bash
# Unit tests (deterministic, no live credentials)
pytest tests/

# All 18 tests pass:
# - Crypto: encrypt/decrypt, deterministic key derivation
# - SSE Reconciler: 7 tests (first chunk, cumulative, duplicate, growing, malformed, [DONE], no negative slicing)
# - Challenge/Error/Transport types: enum existence
# - Request identities: ClientRequestID vs RequestID distinct
# - No automatic retry: in SSE reconciler and session
# - SessionNotReady: carries challenge info
```

## Security

- Browser profile owns credentials; no cookie/token export to Python process
- `_evidence_to_dict()` converts Enums/dataclasses to JSON-serializable primitives
- Sidecar stdout = ONLY JSON-RPC frames; stderr for diagnostics
- No PII in JSON-RPC output

## Known Limitations (Unproven)

- Sentinel/Turnstile handshake reliability (not tested live)
- CDP fetch implementation via `page.evaluate` (simplified; full impl needs CDP Network domain)
- Session warming reliability at scale
- nodriver bypass durability under real anti-bot updates
- No true streaming cancellation (AbortController not implemented)

## References

- PR #77: Research artifact, draft
- Issue #16: Provider abstraction gate
- PR #75: Standalone-substrate evidence gate (merged)
- Skill: `chatgpt-web-provider-hardening`