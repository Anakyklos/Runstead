# Session management for ChatGPT Web

import asyncio
import json
import os
import time
from pathlib import Path
from typing import Optional

import httpx
import nodriver as uc

from chatgptweb.config import settings
from chatgptweb.crypto import decrypt_cookies, encrypt_cookies


class AccountSession:
    """Manages a single ChatGPT Web account session."""

    def __init__(self, account_id: str):
        self.account_id = account_id
        self.account_dir = settings.accounts_dir / account_id
        self.account_dir.mkdir(parents=True, exist_ok=True)
        self.cookies_file = self.account_dir / "cookies.enc"
        self.metadata_file = self.account_dir / "metadata.json"
        self.sentinel_hash_file = self.account_dir / "sentinel_hash.txt"
        self.browser: Optional[uc.Browser] = None
        self.page: Optional[uc.Tab] = None
        self._cookies: dict = {}
        self._access_token: Optional[str] = None
        self._cf_clearance: Optional[str] = None
        self._oai_device_id: Optional[str] = None
        self._last_health_check: float = 0

    async def load_cookies(self) -> bool:
        """Load encrypted cookies from disk."""
        if not self.cookies_file.exists():
            return False
        try:
            data = self.cookies_file.read_bytes()
            self._cookies = decrypt_cookies(data)
            self._extract_key_cookies()
            return True
        except Exception:
            return False

    def _extract_key_cookies(self):
        """Extract key cookies from loaded cookie jar."""
        self._cf_clearance = self._cookies.get("cf_clearance")
        self._oai_device_id = self._cookies.get("oai-device-id")
        self._access_token = self._cookies.get("__Secure-next-auth.session-token")

    async def save_cookies(self):
        """Save cookies to encrypted file."""
        if self.page:
            try:
                cookies = await self.page.get_cookies()
                self._cookies = {c["name"]: c["value"] for c in cookies}
                self._extract_key_cookies()
            except Exception:
                pass

        data = encrypt_cookies(self._cookies)
        self.cookies_file.write_bytes(data)

        metadata = {
            "account_id": self.account_id,
            "last_used": time.time(),
            "has_cf_clearance": bool(self._cf_clearance),
            "has_access_token": bool(self._access_token),
            "has_oai_device_id": bool(self._oai_device_id),
        }
        self.metadata_file.write_text(json.dumps(metadata, indent=2))

    async def start_browser(self):
        """Start nodriver browser."""
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

    async def close_browser(self):
        """Close browser."""
        if self.browser:
            try:
                await self.browser.stop()
            except Exception:
                pass
            self.browser = None
            self.page = None

    async def warm_session(self) -> bool:
        """Warm up session: load cookies or re-authenticate."""
        if await self.load_cookies():
            if await self.validate_session():
                return True
        return await self.reauthenticate()

    async def validate_session(self) -> bool:
        """Validate current session with /api/auth/session."""
        if not self._cookies:
            return False

        try:
            async with httpx.AsyncClient(cookies=self._cookies, timeout=30.0) as client:
                resp = await client.get("https://chatgpt.com/api/auth/session")
                if resp.status_code == 200:
                    data = resp.json()
                    self._access_token = data.get("accessToken")
                    return bool(self._access_token)
        except Exception:
            pass
        return False

    async def reauthenticate(self) -> bool:
        """Full re-authentication via browser."""
        if not self.browser:
            await self.start_browser()

        try:
            self.page = await self.browser.get("https://chatgpt.com")
            await self.page.wait_for_selector("body", timeout=30)
            await asyncio.sleep(5)

            if await self.handle_challenge():
                await asyncio.sleep(3)

            cookies = await self.page.get_cookies()
            self._cookies = {c["name"]: c["value"] for c in cookies}
            self._extract_key_cookies()

            if not self._cf_clearance:
                return False

            if await self.validate_session():
                await self.save_cookies()
                return True

        except Exception as e:
            print(f"Reauth failed: {e}")

        return False

    async def handle_challenge(self) -> bool:
        """Handle Cloudflare Turnstile/CAPTCHA challenge."""
        try:
            iframes = await self.page.query_selector_all("iframe")
            for iframe in iframes:
                src = await iframe.get_attribute("src")
                if src and "turnstile" in src.lower():
                    await iframe.click()
                    await asyncio.sleep(3)
                    return True
        except Exception:
            pass
        return False

    async def probe_drift(self) -> tuple[bool, Optional[str]]:
        """Probe for Sentinel SDK drift."""
        try:
            async with httpx.AsyncClient(cookies=self._cookies, timeout=30.0) as client:
                resp = await client.get("https://chatgpt.com/backend-api/sentinel/sdk.js")
                if resp.status_code == 200:
                    import hashlib
                    current_hash = hashlib.sha256(resp.content).hexdigest()

                    old_hash = None
                    if self.sentinel_hash_file.exists():
                        old_hash = self.sentinel_hash_file.read_text().strip()

                    if old_hash and current_hash != old_hash:
                        return True, f"Drift detected: {old_hash[:16]} -> {current_hash[:16]}"

                    self.sentinel_hash_file.write_text(current_hash)
                    return False, None
        except Exception:
            pass
        return False, None

    async def complete(
        self,
        client_request_id: str,
        model: str,
        messages: list[dict],
        stream: bool = True,
    ):
        """Execute a completion request via SSE."""
        if not self._access_token:
            raise Exception("No access token - session not warm")

        payload = {
            "action": "next",
            "messages": self._format_messages(messages),
            "model": model,
            "conversation_id": None,
            "parent_message_id": None,
            "force_use_sse": True,
            "supported_encodings": ["v1"],
        }

        headers = {
            "accept": "text/event-stream",
            "authorization": f"Bearer {self._access_token}",
            "content-type": "application/json",
            "oai-device-id": self._oai_device_id or "unknown",
        }

        async with httpx.AsyncClient(
            cookies=self._cookies,
            timeout=settings.request_timeout,
        ) as client:
            try:
                async with client.stream(
                    "POST",
                    "https://chatgpt.com/backend-api/conversation",
                    json=payload,
                    headers=headers,
                ) as resp:
                    if resp.status_code != 200:
                        error_text = await resp.aread()
                        raise Exception(f"HTTP {resp.status_code}: {error_text.decode()}")

                    content_buffer = ""
                    last_hash = ""

                    async for line in resp.aiter_lines():
                        if not line or not line.startswith("data: "):
                            continue

                        data_str = line[6:]
                        if data_str == "[DONE]":
                            break

                        try:
                            event = json.loads(data_str)
                            if "message" in event and "content" in event["message"]:
                                parts = event["message"]["content"].get("parts", [])
                                new_content = parts[0] if parts else ""
                                if new_content.startswith(last_hash):
                                    continue
                                last_hash = new_content
                                delta = new_content[len(content_buffer):]
                                content_buffer = new_content
                                yield {"delta": delta, "done": False}
                        except json.JSONDecodeError:
                            continue

                    yield {"delta": "", "done": True, "content": content_buffer}

            except Exception as e:
                raise Exception(f"Completion failed: {e}")

    def _format_messages(self, messages: list[dict]) -> list[dict]:
        """Format messages for ChatGPT Web API."""
        formatted = []
        for msg in messages:
            role = msg.get("role", "user")
            content = msg.get("content", "")
            if role == "system":
                formatted.append({"role": "system", "content": content})
            elif role == "user":
                formatted.append({"role": "user", "content": content})
            elif role == "assistant":
                formatted.append({"role": "assistant", "content": content})
        return formatted


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

    async def warm_account(self, account_id: str) -> bool:
        session = self.get_session(account_id)
        return await session.warm_session()

    async def close_all(self):
        for session in self.sessions.values():
            await session.close_browser()