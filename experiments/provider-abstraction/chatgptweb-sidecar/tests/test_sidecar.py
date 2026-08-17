# Tests for ChatGPT Web Sidecar

import pytest
from unittest.mock import AsyncMock, MagicMock, patch

from chatgptweb.config import Settings
from chatgptweb.crypto import get_encryption_key, encrypt_cookies, decrypt_cookies
from chatgptweb.session import AccountSession, SessionManager


class TestCrypto:
    """Test encryption/decryption."""

    def test_encrypt_decrypt_roundtrip(self):
        """Test that encrypt/decrypt preserves data."""
        cookies = {"cf_clearance": "test-clearance", "session": "test-session"}
        encrypted = encrypt_cookies(cookies)
        decrypted = decrypt_cookies(encrypted)
        assert decrypted == cookies

    def test_get_encryption_key_deterministic(self):
        """Test that key derivation is deterministic."""
        key1 = get_encryption_key()
        key2 = get_encryption_key()
        assert key1 == key2


class TestAccountSession:
    """Test AccountSession logic."""

    @pytest.fixture
    def mock_settings(self):
        with patch("chatgptweb.session.settings") as mock:
            mock.accounts_dir = "/tmp/test-accounts"
            mock.master_key = "test-master-key-32-chars-long!!"
            mock.proxy = None
            mock.headless = True
            mock.request_timeout = 30
            yield mock

    @pytest.mark.asyncio
    async def test_extract_key_cookies(self, mock_settings):
        """Test extraction of key cookies."""
        session = AccountSession("test-account")
        session._cookies = {
            "cf_clearance": "test-cf",
            "oai-device-id": "test-device",
            "__Secure-next-auth.session-token": "test-token",
            "other": "value",
        }
        session._extract_key_cookies()
        assert session._cf_clearance == "test-cf"
        assert session._oai_device_id == "test-device"
        assert session._access_token == "test-token"

    @pytest.mark.asyncio
    async def test_format_messages(self, mock_settings):
        """Test message formatting."""
        session = AccountSession("test-account")
        messages = [
            {"role": "system", "content": "You are helpful"},
            {"role": "user", "content": "Hello"},
            {"role": "assistant", "content": "Hi there"},
        ]
        formatted = session._format_messages(messages)
        assert len(formatted) == 3
        assert formatted[0]["role"] == "system"
        assert formatted[1]["role"] == "user"
        assert formatted[2]["role"] == "assistant"


class TestSessionManager:
    """Test SessionManager."""

    @pytest.fixture
    def mock_settings(self):
        with patch("chatgptweb.session.settings") as mock:
            mock.accounts_dir = "/tmp/test-accounts"
            mock.master_key = "test-master-key-32-chars-long!!"
            mock.proxy = None
            mock.headless = True
            yield mock

    def test_get_session_creates_new(self, mock_settings):
        """Test that get_session creates new session."""
        manager = SessionManager()
        session = manager.get_session("account-1")
        assert isinstance(session, AccountSession)
        assert session.account_id == "account-1"
        # Second call returns same instance
        session2 = manager.get_session("account-1")
        assert session is session2

    def test_list_accounts_empty(self, mock_settings):
        """Test listing accounts when directory doesn't exist."""
        manager = SessionManager()
        accounts = manager.list_accounts()
        assert accounts == []


# Run with: pytest tests/test_sidecar.py -v
if __name__ == "__main__":
    pytest.main([__file__, "-v"])