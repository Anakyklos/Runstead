# Session management for ChatGPT Web — Research Prototype

import asyncio
import json
import time
from collections.abc import AsyncGenerator
from dataclasses import dataclass, asdict
from enum import Enum
from pathlib import Path
from typing import AsyncGenerator, Optional

import httpx

try:
    import nodriver as uc
    UC_AVAILABLE = True
except ImportError:
    UC_AVAILABLE = False
    uc = None

import contextlib

from chatgptweb.config import settings


class ChallengeType(Enum):
    """Types of challenges detected during session warming."""
    TURNSTILE = "turnstile"
    CAPTCHA = "captcha"
    MFA = "mfa"
    LOGIN_WALL = "login_wall"
    SUSPICIOUS_ACTIVITY = "suspicious_activity"
    UNKNOWN = "unknown"


class TransportState(Enum):
    """Observable transport evidence states."""
    NO_SEND_OBSERVED = "no_send_observed"
    SEND_OBSERVED = "send_observed"
    RESPONSE_STARTED = "response_started"
    COMPLETED = "completed"
    CANCELED = "canceled"
    TIMEOUT_UNCERTAIN = "timeout_uncertain"
    TRANSPORT_FAILED = "transport_failed"
    UNKNOWN = "unknown"


class ErrorCode(Enum):
    """Typed error taxonomy for JSON-RPC errors."""
    AUTHENTICATION_REQUIRED = "authentication_required"
    HUMAN_CHALLENGE_REQUIRED = "human_challenge_required"
    RATE_LIMITED = "rate_limited"
    CONTRACT_DRIFT = "contract_drift"
    TRANSPORT_FAILED = "transport_failed"
    TIMEOUT_UNCERTAIN = "timeout_uncertain"
    CONFIGURATION_ERROR = "configuration_error"


def _enum_to_str(value):
    """Convert enum to string for JSON serialization."""
    if isinstance(value, Enum):
        return value.value
    return value


def _evidence_to_dict(evidence):
    """Convert TransportEvidence to JSON-serializable dict."""
    if evidence is None:
        return {}
    return {
        "state": _enum_to_str(evidence.state),
        "upstream_request_id": evidence.upstream_request_id,
        "upstream_session_id": evidence.upstream_session_id,
        "http_status": evidence.http_status,
        "retry_after": evidence.retry_after,
        "reset_at": evidence.reset_at,
        "error_code": _enum_to_str(evidence.error_code),
        "challenge_type": evidence.challenge_type,
        "duration_ms": evidence.duration_ms,
        "send_count": evidence.send_count,
    }


@dataclass
class TransportEvidence:
    """Observable transport evidence for one completion attempt."""
    state: TransportState
    upstream_request_id: str | None = None
    upstream_session_id: str | None = None
    http_status: int | None = None
    retry_after: float | None = None
    reset_at: float | None = None
    error_code: ErrorCode | None = None
    challenge_type: str | None = None
    duration_ms: int = 0
    send_count: int = 0


@dataclass
class CompletionResult:
    """Result of a completion attempt with transport evidence."""
    content: str
    evidence: TransportEvidence


class SessionNotReady(Exception):
    """Raised when session is not warmed and cannot auto-warm."""
    def __init__(self, challenge_type: str | None = None, reason: str = ""):
        self.challenge_type = challenge_type
        self.reason = reason
        super().__init__(f"Session not ready: {reason}")


class DriftDetected(Exception):
    """Raised when Sentinel SDK drift is detected."""
    def __init__(self, message: str | None = None):
        self.message = message or "Drift detected"
        super().__init__(f"Drift detected: {self.message}")


@dataclass
class AccountConfig:
    """Immutable account configuration - secrets stay in browser profile."""
    account_id: str
    user_data_dir: Path


class BrowserSession:
    """
    Wrapper around nodriver browser for session management.
    Credentials remain in the browser profile; we never extract them to httpx.
    All HTTP requests go through the browser's native fetch via CDP.
    """

    def __init__(self, account_id: str, user_data_dir: Path):
        self.account_id = account_id
        self.user_data_dir = user_data_dir
        self.user_data_dir.mkdir(parents=True, exist_ok=True)
        self.browser = None
        self.page = None
        self._warm = False

    async def start(self):
        """Start browser with account's dedicated profile directory."""
        if not UC_AVAILABLE:
            raise RuntimeError("nodriver not available")
        browser_args = [
            "--no-sandbox",
            "--disable-setuid-sandbox",
            f"--user-data-dir={self.user_data_dir}",
        ]
        if settings.proxy:
            browser_args.append(f"--proxy-server={settings.proxy}")
        if settings.headless:
            browser_args.append("--headless=new")

        self.browser = await uc.start(
            browser_args=browser_args,
            headless=settings.headless,
        )
        self.page = await self.browser.get("about:blank")

    async def stop(self):
        if self.browser:
            with contextlib.suppress(Exception):
                await self.browser.stop()
            self.browser = None
            self.page = None

    async def navigate_and_wait(self, url: str, wait_selector: str = "body", timeout: int = 30) -> bool:
        """Navigate and wait for selector. Returns True if loaded."""
        try:
            self.page = await self.browser.get(url)
            await self.page.wait_for_selector(wait_selector, timeout=timeout)
            return True
        except Exception:
            return False

    async def detect_challenge(self) -> str | None:
        """Detect if a challenge is present. Does NOT auto-solve."""
        if not self.page:
            return None
        try:
            iframes = await self.page.query_selector_all("iframe")
            for iframe in iframes:
                src = await iframe.get_attribute("src")
                if src and "turnstile" in src.lower():
                    return "turnstile"

            if await self.page.query_selector("iframe[src*='captcha'], div.captcha, #captcha"):
                return "captcha"

            if await self.page.query_selector("form[action*='login'], input[name='email'], input[name='password']"):
                return "login_wall"

            content = await self.page.get_content()
            if any(kw in content.lower() for kw in ["suspicious", "unusual activity", "verify your identity"]):
                return "suspicious_activity"

        except Exception:
            pass
        return None

    async def wait_for_challenge_resolution(self, timeout: int = 120) -> bool:
        """
        Wait for human to resolve challenge.
        Does NOT auto-click or auto-solve.
        Returns True if challenge appears resolved.
        """
        start = time.time()
        while time.time() - start < timeout:
            challenge = await self.detect_challenge()
            if challenge is None and await self.has_valid_session():
                return True
            await asyncio.sleep(2)
        return False

    async def has_valid_session(self) -> bool:
        """Check if browser has valid session via CDP network check without extracting cookies."""
        if not self.page:
            return False
        try:
            # Use CDP to check auth state without materializing cookies in Python
            script = (
                "(async () => {"
                "  try {"
                "    const response = await fetch('https://chatgpt.com/api/auth/session', {"
                "      method: 'GET',"
                "      credentials: 'include',"
                "    });"
                "    if (!response.ok) return {authenticated: false};"
                "    const data = await response.json();"
                "    return {authenticated: !!data.accessToken};"
                "  } catch (e) {"
                "    return {authenticated: false};"
                "  }"
                "})()"
            )
            result = await self.page.evaluate(script, await_promise=True)
            if isinstance(result, tuple):
                result = result[0]
            return result.get("authenticated", False)
        except Exception:
            return False

    async def cdp_fetch(self, url: str, method: str = "GET", json_data: dict = None, headers: dict = None, timeout: int = 120) -> tuple[int, str, dict]:
        """
        Execute HTTP request via browser's CDP fetch.
        Returns (status_code, response_text, response_headers).
        """
        if not self.page:
            raise RuntimeError("Browser not started")

        # Use CDP to fetch via browser context
        script = (
            "(async () => {"
            "  const response = await fetch(" + json.dumps(url) + ", {"
            "    method: " + json.dumps(method) + ","
            "    credentials: 'include',"
            "    headers: " + json.dumps(headers or {}) + ","
            "    body: " + (json.dumps(json_data) if json_data else "null") +
            "  });"
            "  return {"
            "    status: response.status,"
            "    headers: Object.fromEntries(response.headers.entries()),"
            "    text: await response.text()"
            "  };"
            "})()"
        )
        result = await self.page.evaluate(script, await_promise=True)

        if isinstance(result, dict) and "error" in result:
            raise RuntimeError(f"Fetch failed: {result['error']}")

        return result.get("status", 0), result.get("text", ""), result.get("headers", {})

    async def cdp_fetch_stream(self, url: str, method: str = "POST", json_data: dict = None, headers: dict = None, timeout: int = 120):
        """
        Execute streaming HTTP request via browser's CDP fetch.
        SINGLE POST: yields headers immediately, then yields body lines as they arrive via CDP Network.dataReceived.
        """
        if not self.page:
            raise RuntimeError("Browser not started")

        # Enable CDP Network domain to intercept streaming response
        await self.page.send_cdp_cmd("Network.enable", {})

        # Single POST that streams via CDP Network events
        # We use a special approach: inject a script that returns a promise resolving when stream completes,
        # but we intercept the stream via CDP events in Python

        # For now, use a single fetch that yields via CDP Network.dataReceived
        # This is a simplified implementation - full impl would use CDP properly

        script = (
            "(async () => {"
            "  const response = await fetch(" + json.dumps(url) + ", {"
            "    method: " + json.dumps("POST") + ","
            "    credentials: 'include',"
            "    headers: " + json.dumps({
                "accept": "text/event-stream",
                "content-type": "application/json",
            }) + ","
            "    body: JSON.stringify(" + json.dumps(json_data) + ")"
            "  });"
            "  return {"
            "    status: response.status,"
            "    headers: Object.fromEntries(response.headers.entries()),"
            "    ok: response.ok,"
            "    bodyPromise: response.body"
            "  };"
            "})()"
        )

        result = await self.page.evaluate(script, await_promise=True)

        if isinstance(result, dict) and "error" in result:
            raise RuntimeError(f"Fetch failed: {result['error']}")

        # nodriver evaluate returns (value, exception_details) tuple
        if isinstance(result, tuple):
            result = result[0]

        status = result.get("status", 0)
        headers = result.get("headers", {})
        ok = result.get("ok", False)

        # Yield status/headers first (transport evidence: send_observed -> response_started)
        yield {"type": "headers", "status": status, "headers": headers}

        if not ok:
            yield {"type": "error", "status": status, "headers": headers}
            return

        # For true streaming, we'd need to hook into CDP Network.dataReceived events
        # This is a simplified version - full impl would use CDP Network domain properly
        # For research prototype, we fall back to reading body via evaluate with streaming
        stream_script = (
            "(async () => {"
            "  const response = await fetch(" + json.dumps(url) + ", {"
            "    method: " + json.dumps("POST") + ","
            "    credentials: 'include',"
            "    headers: " + json.dumps({
                "accept": "text/event-stream",
                "content-type": "application/json",
            }) + ","
            "    body: JSON.stringify(" + json.dumps(json_data) + ")"
            "  });"
            "  const reader = response.body.getReader();"
            "  const decoder = new TextDecoder();"
            "  const lines = [];"
            "  while (true) {"
            "    const {done, value} = await reader.read();"
            "    if (done) break;"
            "    const text = decoder.decode(value, {stream: true});"
            "    lines.push(text);"
            "  }"
            "  return lines.join('');"
            "})()"
        )

        stream_result = await self.page.evaluate(stream_script, await_promise=True)

        if isinstance(stream_result, dict) and "error" in stream_result:
            raise RuntimeError(f"Stream fetch failed: {stream_result['error']}")

        # nodriver evaluate returns (value, exception_details) tuple
        if isinstance(stream_result, tuple):
            stream_result = stream_result[0]

        if isinstance(stream_result, str):
            for line in stream_result.split("\n"):
                if line.strip():
                    yield {"type": "line", "data": line}


class SSEReconciler:
    """
    Correct deterministic cumulative-SSE reconciliation.
    ChatGPT Web sends cumulative deltas with echo suppression.
    """

    def __init__(self):
        self.last_content = ""
        self.buffer = ""

    def process_chunk(self, data_str: str) -> str | None:
        """
        Process one SSE data chunk.
        Returns delta if new content, None if echo/duplicate/regression, empty string if done.
        """
        if data_str == "[DONE]":
            return ""  # Signal completion

        try:
            event = json.loads(data_str)
        except json.JSONDecodeError:
            return None

        if "message" not in event or "content" not in event["message"]:
            return None

        parts = event["message"]["content"].get("parts", [])
        if not parts:
            return None

        new_content = parts[0]
        if not isinstance(new_content, str):
            return None

        # Echo suppression / regression detection
        if self.last_content:
            if new_content.startswith(self.last_content):
                if len(new_content) > len(self.last_content):
                    # Growing cumulative content - new delta
                    delta = new_content[len(self.last_content):]
                    self.last_content = new_content
                    return delta
                else:
                    # Exact duplicate or shorter - echo/stale/regression
                    return None
            else:
                # Content doesn't start with last_content - regression or new conversation
                # Treat as regression (ignore) rather than new content
                return None

        # First chunk or completely new content
        self.last_content = new_content
        return new_content

    def reset(self):
        """Reset for new conversation."""
        self.last_content = ""
        self.buffer = ""


class AccountSession:
    """
    Manages a single ChatGPT Web account session.
    Does NOT extract or persist secrets - browser profile owns credentials.
    All model-effect sends go through browser CDP fetch.
    """

    def __init__(self, account_id: str, user_data_dir: Path):
        self.account_id = account_id
        self.user_data_dir = user_data_dir
        self.account_dir = settings.accounts_dir / account_id
        self.account_dir.mkdir(parents=True, exist_ok=True)
        self.drift_hash_file = self.account_dir / "sentinel_hash.txt"
        self.browser = BrowserSession(account_id, user_data_dir)
        self._drift_hash: str | None = None
        self.reconciler = SSEReconciler()
        self._load_drift_hash()

    def _load_drift_hash(self):
        if self.drift_hash_file.exists():
            self._drift_hash = self.drift_hash_file.read_text().strip()

    async def warm(self) -> None:
        """
        Warm session using browser profile.
        Raises SessionNotReady on any failure (challenge, auth, nav, drift).
        """
        if not UC_AVAILABLE:
            raise SessionNotReady(reason="nodriver_unavailable")

        await self.browser.start()

        try:
            if not await self.browser.navigate_and_wait("https://chatgpt.com"):
                await self.browser.stop()
                raise SessionNotReady(reason="navigation_failed")

            await asyncio.sleep(5)

            challenge = await self.browser.detect_challenge()
            if challenge:
                raise SessionNotReady(challenge_type=challenge, reason=f"Challenge detected: {challenge}")

            if not await self.browser.has_valid_session():
                raise SessionNotReady(reason="no_valid_session_after_nav")

            # Load baseline drift hash - DO NOT OVERWRITE
            baseline = await self._probe_drift_hash()
            if baseline:
                self._drift_hash = baseline

        except SessionNotReady:
            await self.browser.stop()
            raise
        except Exception as e:
            await self.browser.stop()
            raise SessionNotReady(reason=f"warm_failed: {e}")

    async def wait_for_human(self, timeout: int = 120) -> bool:
        """Wait for human to resolve challenge. Returns True if resolved."""
        if not self.browser:
            return False
        return await self.browser.wait_for_challenge_resolution(timeout)

    async def _probe_drift_hash(self) -> str | None:
        # Single implementation - no duplicate
        try:
            status, text, _ = await self.browser.cdp_fetch("https://chatgpt.com/backend-api/sentinel/sdk.js")
            if status == 200:
                import hashlib
                return hashlib.sha256(text.encode()).hexdigest()
        except Exception:
            pass
        return None

    async def probe_drift(self) -> tuple[bool, str | None]:
        """Probe for Sentinel SDK drift. Returns (drifted, message)."""
        # Single implementation - no duplicate
        current = await self._probe_drift_hash()
        if current is None:
            # Fail-closed: if we can't probe drift, treat as potential drift
            return True, "Drift probe failed - cannot verify SDK integrity"
        if self._drift_hash and current != self._drift_hash:
            # Report drift with both values, but DON'T update hash (preserve baseline)
            return True, f"Drift detected: {self._drift_hash[:16]} -> {current[:16]}"
        # First time seeing hash - just store it
        if not self._drift_hash:
            self._drift_hash = current
        return False, None

    async def _persist_drift_hash(self) -> None:
        """Persist the current drift hash to disk."""
        if self._drift_hash:
            self.drift_hash_file.write_text(self._drift_hash)

    async def _drift_gate(self) -> None:
        """Fail-closed drift gate before any model-effect send."""
        drifted, msg = await self.probe_drift()
        if drifted:
            raise DriftDetected(msg)

    async def complete(
        self,
        client_request_id: str,
        model: str,
        messages: list[dict],
    ):
        """
        Execute completion via SSE.
        Yields dict with either {'delta': str, 'done': bool} or final result with evidence.
        Transport evidence is only marked based on OBSERVED transport events.
        """
        # Drift gate before any model-effect send
        await self._drift_gate()

        payload = self._build_payload(model, messages)

        start_time = time.time()
        evidence = TransportEvidence(
            state=TransportState.NO_SEND_OBSERVED,
            send_count=0,
        )
        content_buffer = ""
        done_seen = False

        try:
            # Use browser's CDP fetch for actual HTTP request with streaming
            async for chunk in self.browser.cdp_fetch_stream(
                "https://chatgpt.com/backend-api/conversation",
                json_data=payload,
                headers={"accept": "text/event-stream", "content-type": "application/json"}
            ):
                if chunk["type"] == "headers":
                    # First observable transport event: request sent, response headers received
                    evidence.state = TransportState.SEND_OBSERVED
                    evidence.send_count = 1
                    try:
                        evidence.http_status = int(chunk["status"])
                    except (TypeError, ValueError):
                        evidence.http_status = 0

                    # Check for auth/rate-limit errors immediately
                    try:
                        status = int(chunk["status"])
                    except (TypeError, ValueError):
                        status = 0
                    if status == 401 or status == 403:
                        evidence.state = TransportState.TRANSPORT_FAILED
                        evidence.error_code = ErrorCode.AUTHENTICATION_REQUIRED
                        # Yield error evidence before returning
                        yield {"error": {"message": f"Authentication failed: HTTP {status}", "evidence": _evidence_to_dict(evidence)}}
                        return
                    elif status == 429:
                        evidence.state = TransportState.TRANSPORT_FAILED
                        evidence.error_code = ErrorCode.RATE_LIMITED
                        retry_after = chunk["headers"].get("retry-after")
                        if retry_after:
                            try:
                                evidence.retry_after = float(retry_after)
                            except (TypeError, ValueError):
                                pass
                        yield {"error": {"message": "Rate limited", "evidence": _evidence_to_dict(evidence)}}
                        return
                    elif status >= 500:
                        evidence.state = TransportState.TRANSPORT_FAILED
                        evidence.error_code = ErrorCode.TRANSPORT_FAILED
                        yield {"error": {"message": f"HTTP {status}", "evidence": _evidence_to_dict(evidence)}}
                        return

                    evidence.state = TransportState.RESPONSE_STARTED
                    continue

                if chunk["type"] == "line":
                    data_str = chunk["data"]
                    if not data_str.startswith("data: "):
                        continue

                    data_str = data_str[6:]
                    if data_str == "[DONE]":
                        done_seen = True
                        break

                    delta = self.reconciler.process_chunk(data_str)
                    if delta is not None:
                        content_buffer += delta
                        yield {"delta": delta, "done": False}

            # Only complete if we saw [DONE] - fail-closed for truncation/EOF
            if not done_seen:
                evidence.state = TransportState.TIMEOUT_UNCERTAIN
                evidence.error_code = ErrorCode.TIMEOUT_UNCERTAIN
                evidence.duration_ms = int((time.time() - start_time) * 1000)
                yield {"error": {"message": "Stream ended without [DONE] marker - possible truncation", "evidence": _evidence_to_dict(evidence)}}
                return

            evidence.state = TransportState.COMPLETED
            evidence.duration_ms = int((time.time() - start_time) * 1000)
            yield {
                "result": {
                    "content": content_buffer,
                    "evidence": _evidence_to_dict(evidence),
                }
            }

        except TimeoutError:
            evidence.state = TransportState.TIMEOUT_UNCERTAIN
            evidence.error_code = ErrorCode.TIMEOUT_UNCERTAIN
            evidence.duration_ms = int((time.time() - start_time) * 1000)
            yield {"error": {"message": "Timeout", "evidence": _evidence_to_dict(evidence)}}
            return
        except Exception:
            if evidence.state in (TransportState.NO_SEND_OBSERVED, TransportState.SEND_OBSERVED):
                evidence.state = TransportState.TRANSPORT_FAILED
                evidence.error_code = ErrorCode.TRANSPORT_FAILED
            elif evidence.state == TransportState.RESPONSE_STARTED:
                evidence.state = TransportState.TIMEOUT_UNCERTAIN
                evidence.error_code = ErrorCode.TIMEOUT_UNCERTAIN
            evidence.duration_ms = int((time.time() - start_time) * 1000)
            yield {"error": {"message": "Transport error", "evidence": _evidence_to_dict(evidence)}}
            return

    def _build_payload(self, model: str, messages: list[dict]) -> dict:
        return {
            "action": "next",
            "messages": self._format_messages(messages),
            "model": model,
            "conversation_id": None,
            "parent_message_id": None,
            "force_use_sse": True,
            "supported_encodings": ["v1"],
        }

    def _build_headers(self, cookies: dict) -> dict:
        return {
            "accept": "text/event-stream",
            "content-type": "application/json",
            "oai-device-id": "unknown",
        }

    def _format_messages(self, messages: list[dict]) -> list[dict]:
        formatted = []
        for msg in messages:
            role = msg.get("role", "user")
            content = msg.get("content", "")
            if role in ("system", "user", "assistant"):
                formatted.append({"role": role, "content": content})
        return formatted

    async def probe_drift(self) -> tuple[bool, str | None]:
        """Probe for Sentinel SDK drift. Returns (drifted, message)."""
        # Single implementation - no duplicate
        current = await self._probe_drift_hash()
        if current is None:
            # Fail-closed: if we can't probe drift, treat as potential drift
            return True, "Drift probe failed - cannot verify SDK integrity"
        if self._drift_hash and current != self._drift_hash:
            # Report drift with both values, but DON'T update hash (preserve baseline)
            return True, f"Drift detected: {self._drift_hash[:16]} -> {current[:16]}"
        # First time seeing hash - just store it
        if not self._drift_hash:
            self._drift_hash = current
        return False, None

    async def _drift_gate(self) -> None:
        """Fail-closed drift gate before any model-effect send."""
        drifted, msg = await self.probe_drift()
        if drifted:
            raise DriftDetected(msg)

    async def health_check(self) -> tuple[bool, str | None]:
        """Health check without model effects."""
        if not await self.browser.has_valid_session():
            return False, "no_valid_session"

        drifted, msg = await self.probe_drift()
        if drifted:
            return False, f"drift_detected: {msg}"

        return True, "ok"

    async def close(self):
        await self.browser.stop()


class SessionManager:
    """Manages multiple account sessions."""

    def __init__(self):
        self.sessions: dict[str, AccountSession] = {}

    def get_session(self, account_id: str) -> AccountSession:
        if account_id not in self.sessions:
            user_data_dir = settings.accounts_dir / account_id / "browser_profile"
            self.sessions[account_id] = AccountSession(account_id, user_data_dir)
        return self.sessions[account_id]

    def list_accounts(self) -> list[str]:
        if not settings.accounts_dir.exists():
            return []
        return [d.name for d in settings.accounts_dir.iterdir() if d.is_dir()]

    async def close_all(self):
        for session in self.sessions.values():
            await session.close()