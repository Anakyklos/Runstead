# Session management for ChatGPT Web — Research Prototype

import asyncio
import json
import time
from collections.abc import AsyncGenerator
from dataclasses import dataclass
from enum import Enum

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
    def __init__(self, message: str):
        self.message = message
        super().__init__(f"Drift detected: {message}")


@dataclass
class AccountConfig:
    """Immutable account configuration - secrets stay in browser profile."""
    account_id: str


class BrowserSession:
    """
    Wrapper around nodriver browser for session management.
    Credentials remain in the browser profile; we never extract them.
    """

    def __init__(self, account_id: str):
        self.account_id = account_id
        self.browser = None
        self.page = None
        self._warm = False

    async def start(self):
        """Start browser with account's profile."""
        if not UC_AVAILABLE:
            raise RuntimeError("nodriver not available")
        browser_args = ["--no-sandbox", "--disable-setuid-sandbox"]
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
            # Check for Turnstile iframe
            iframes = await self.page.query_selector_all("iframe")
            for iframe in iframes:
                src = await iframe.get_attribute("src")
                if src and "turnstile" in src.lower():
                    return "turnstile"

            # Check for CAPTCHA
            if await self.page.query_selector("iframe[src*='captcha'], div.captcha, #captcha"):
                return "captcha"

            # Check for login wall
            if await self.page.query_selector("form[action*='login'], input[name='email'], input[name='password']"):
                return "login_wall"

            # Check for suspicious activity banner
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
        """Check if browser has valid session cookies."""
        if not self.page:
            return False
        try:
            cookies = await self.page.get_cookies()
            has_cf_clearance = any(c.get("name") == "cf_clearance" for c in cookies)
            return has_cf_clearance
        except Exception:
            return False

    async def get_cookies_for_transport(self) -> dict:
        """Get cookies for httpx transport - ONLY non-secret cookies."""
        if not self.page:
            return {}
        try:
            cookies = await self.page.get_cookies()
            allowed = {"cf_clearance", "oai-device-id"}
            return {c.get("name"): c.get("value") for c in cookies if c.get("name") in allowed}
        except Exception:
            return {}


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
    """

    def __init__(self, account_id: str):
        self.account_id = account_id
        self.account_dir = settings.accounts_dir / account_id
        self.account_dir.mkdir(parents=True, exist_ok=True)
        self.drift_hash_file = self.account_dir / "sentinel_hash.txt"
        self.browser = BrowserSession(account_id)
        self._drift_hash: str | None = None
        self._load_drift_hash()

    def _load_drift_hash(self):
        if self.drift_hash_file.exists():
            self._drift_hash = self.drift_hash_file.read_text().strip()

    async def warm(self) -> tuple[bool, str | None]:
        """
        Warm session using browser profile.
        Returns (success, challenge_type_if_any).
        """
        if not UC_AVAILABLE:
            return False, "nodriver_unavailable"

        await self.browser.start()

        if not await self.browser.navigate_and_wait("https://chatgpt.com"):
            await self.browser.stop()
            return False, "navigation_failed"

        await asyncio.sleep(5)

        challenge = await self.browser.detect_challenge()
        if challenge:
            return False, challenge

        if await self.browser.has_valid_session():
            self._drift_hash = await self._probe_drift_hash()
            return True, None

        return False, "unknown"

    async def wait_for_human(self, timeout: int = 120) -> bool:
        """Wait for human to resolve challenge. Returns True if resolved."""
        if not self.browser:
            return False
        return await self.browser.wait_for_challenge_resolution(timeout)

    async def _probe_drift_hash(self) -> str | None:
        try:
            cookies = await self.browser.get_cookies_for_transport()
            async with httpx.AsyncClient(cookies=cookies, timeout=30.0) as client:
                resp = await client.get("https://chatgpt.com/backend-api/sentinel/sdk.js")
                if resp.status_code == 200:
                    import hashlib
                    return hashlib.sha256(resp.content).hexdigest()
        except Exception:
            pass
        return None

    async def probe_drift(self) -> tuple[bool, str | None]:
        """Probe for Sentinel SDK drift. Returns (drifted, message)."""
        current = await self._probe_drift_hash()
        if current and self._drift_hash and current != self._drift_hash:
            self._drift_hash = current
            return True, f"Drift detected: {self._drift_hash[:16]} -> {current[:16]}"
        if current:
            self._drift_hash = current
        return False, None

    async def complete(
        self,
        client_request_id: str,
        model: str,
        messages: list[dict],
    ) -> AsyncGenerator[dict, None]:
        """
        Execute completion via SSE.
        Yields dict with either {'delta': str, 'done': bool} or final result with evidence.
        """
        cookies = await self.browser.get_cookies_for_transport()
        if not cookies.get("cf_clearance"):
            raise SessionNotReady(reason="No cf_clearance - session not warm")

        payload = self._build_payload(model, messages)
        headers = self._build_headers(cookies)

        start_time = time.time()
        evidence = {
            "state": "no_send_observed",
            "upstream_request_id": None,
            "upstream_session_id": None,
            "http_status": None,
            "retry_after": None,
            "reset_at": None,
            "error_code": None,
            "challenge_type": None,
            "duration_ms": 0,
            "send_count": 0,
        }

        async with httpx.AsyncClient(
            cookies=cookies,
            timeout=settings.request_timeout,
        ) as client:
            try:
                evidence["state"] = "send_observed"
                evidence["send_count"] = 1

                async with client.stream(
                    "POST",
                    "https://chatgpt.com/backend-api/conversation",
                    json=payload,
                    headers=headers,
                ) as resp:
                    evidence["http_status"] = resp.status_code

                    if resp.status_code == 429:
                        evidence["error_code"] = "rate_limited"
                        retry_after = resp.headers.get("Retry-After")
                        if retry_after:
                            evidence["retry_after"] = float(retry_after)
                        raise SessionNotReady(reason="Rate limited")

                    if resp.status_code >= 500:
                        evidence["error_code"] = "transport_failed"
                        raise Exception(f"HTTP {resp.status_code}")

                    evidence["state"] = "response_started"

                    reconciler = SSEReconciler()
                    content_buffer = ""

                    async for line in resp.aiter_lines():
                        if not line or not line.startswith("data: "):
                            continue

                        data_str = line[6:]
                        delta = reconciler.process_chunk(data_str)

                        if delta == "":
                            break

                        if delta is not None:
                            content_buffer += delta
                            evidence["state"] = "response_started"
                            yield {"delta": delta, "done": False}

                    evidence["state"] = "completed"
                    evidence["duration_ms"] = int((time.time() - start_time) * 1000)

                    yield {
                        "result": {
                            "content": content_buffer,
                            "evidence": evidence,
                        }
                    }

            except TimeoutError:
                evidence["state"] = "timeout_uncertain"
                evidence["error_code"] = "timeout_uncertain"
                evidence["duration_ms"] = int((time.time() - start_time) * 1000)
                raise
            except Exception:
                if evidence["state"] in ("no_send_observed", "send_observed"):
                    evidence["state"] = "transport_failed"
                    evidence["error_code"] = "transport_failed"
                elif evidence["state"] == "response_started":
                    evidence["state"] = "timeout_uncertain"
                    evidence["error_code"] = "timeout_uncertain"
                evidence["duration_ms"] = int((time.time() - start_time) * 1000)
                raise

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
            "oai-device-id": cookies.get("oai-device-id", "unknown"),
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
        try:
            cookies = await self.browser.get_cookies_for_transport()
            async with httpx.AsyncClient(cookies=cookies, timeout=30.0) as client:
                resp = await client.get("https://chatgpt.com/backend-api/sentinel/sdk.js")
                if resp.status_code == 200:
                    import hashlib
                    current = hashlib.sha256(resp.content).hexdigest()
                    if hasattr(self, '_drift_hash') and self._drift_hash and current != self._drift_hash:
                        self._drift_hash = current
                        return True, f"Drift: {self._drift_hash[:16]} -> {current[:16]}"
                    self._drift_hash = current
        except Exception:
            pass
        return False, None

    async def health_check(self) -> tuple[bool, str | None]:
        """Health check without model effects."""
        try:
            if not await self.browser.has_valid_session():
                return False, "no_valid_session"

            drifted, msg = await self.probe_drift()
            if drifted:
                return False, f"drift_detected: {msg}"

            return True, "ok"
        except Exception as e:
            return False, f"health_check_failed: {e}"

    async def close(self):
        await self.browser.stop()


class SessionManager:
    """Manages multiple account sessions."""

    def __init__(self):
        self.sessions: dict[str, AccountSession] = {}

    def get_session(self, account_id: str) -> AccountSession:
        if account_id not in self.sessions:
            self.sessions[account_id] = AccountSession(account_id)
        return self.sessions[account_id]

    def list_accounts(self) -> list[str]:
        if not settings.accounts_dir.exists():
            return []
        return [d.name for d in settings.accounts_dir.iterdir() if d.is_dir()]

    async def close_all(self):
        for session in self.sessions.values():
            await session.close()
