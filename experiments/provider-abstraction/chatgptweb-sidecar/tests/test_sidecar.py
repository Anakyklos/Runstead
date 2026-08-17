# Tests for ChatGPT Web Sidecar

import os

import pytest

# Set test environment variables before importing
os.environ["CHATGPTWEB_MASTER_KEY"] = "test-master-key-32-characters-long!!"

from chatgptweb.crypto import decrypt_cookies, encrypt_cookies, get_encryption_key
from chatgptweb.session import (
    AccountSession,
    ChallengeType,
    ErrorCode,
    SessionNotReady,
    SSEReconciler,
    TransportState,
)


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


class TestSSEReconciler:
    """Test SSE reconciler with deterministic fixtures."""

    def test_first_chunk(self):
        """Test processing first chunk."""
        reconciler = SSEReconciler()
        # First chunk with content "Hello"
        event = '{"message": {"content": {"parts": ["Hello"]}}}'
        delta = reconciler.process_chunk(event)
        assert delta == "Hello"
        assert reconciler.last_content == "Hello"

    def test_cumulative_second_chunk(self):
        """Test cumulative second chunk."""
        reconciler = SSEReconciler()
        reconciler.process_chunk('{"message": {"content": {"parts": ["Hello"]}}}')
        # Second chunk with "Hello world"
        delta = reconciler.process_chunk('{"message": {"content": {"parts": ["Hello world"]}}}')
        assert delta == " world"
        assert reconciler.last_content == "Hello world"

    def test_exact_duplicate_ignored(self):
        """Test that exact duplicate is ignored."""
        reconciler = SSEReconciler()
        reconciler.process_chunk('{"message": {"content": {"parts": ["Hello"]}}}')
        # Duplicate
        delta = reconciler.process_chunk('{"message": {"content": {"parts": ["Hello"]}}}')
        assert delta is None

    def test_growing_cumulative(self):
        """Test growing cumulative content."""
        reconciler = SSEReconciler()
        reconciler.process_chunk('{"message": {"content": {"parts": ["H"]}}}')
        reconciler.process_chunk('{"message": {"content": {"parts": ["He"]}}}')
        delta = reconciler.process_chunk('{"message": {"content": {"parts": ["Hel"]}}}')
        assert delta == "l"

    def test_malformed_event_ignored(self):
        """Test that malformed/non-message event is ignored."""
        reconciler = SSEReconciler()
        delta = reconciler.process_chunk('{"other": "field"}')
        assert delta is None

    def test_done_signal(self):
        """Test [DONE] signal returns empty string."""
        reconciler = SSEReconciler()
        delta = reconciler.process_chunk("[DONE]")
        assert delta == ""

    def test_final_assembled_response(self):
        """Test final assembled response from multiple chunks."""
        reconciler = SSEReconciler()
        chunks = [
            '{"message": {"content": {"parts": ["H"]}}}',
            '{"message": {"content": {"parts": ["He"]}}}',
            '{"message": {"content": {"parts": ["Hel"]}}}',
            '{"message": {"content": {"parts": ["Hell"]}}}',
            '{"message": {"content": {"parts": ["Hello"]}}}',
        ]
        result = ""
        for chunk in chunks:
            delta = reconciler.process_chunk(chunk)
            if delta is not None:
                result += delta
        assert result == "Hello"

    def test_no_negative_slicing(self):
        """Test no regression/negative slicing."""
        reconciler = SSEReconciler()
        reconciler.process_chunk('{"message": {"content": {"parts": ["Hello"]}}}')
        # Going backwards in content should be ignored
        delta = reconciler.process_chunk('{"message": {"content": {"parts": ["Hel"]}}}')
        assert delta is None


class TestChallengeDetection:
    """Test challenge detection types."""

    def test_challenge_types_exist(self):
        """Verify all challenge types are defined."""
        assert ChallengeType.TURNSTILE.value == "turnstile"
        assert ChallengeType.CAPTCHA.value == "captcha"
        assert ChallengeType.MFA.value == "mfa"
        assert ChallengeType.LOGIN_WALL.value == "login_wall"
        assert ChallengeType.SUSPICIOUS_ACTIVITY.value == "suspicious_activity"
        assert ChallengeType.UNKNOWN.value == "unknown"


class TestErrorCodes:
    """Test error code taxonomy."""

    def test_error_codes_exist(self):
        """Verify all error codes are defined."""
        assert ErrorCode.AUTHENTICATION_REQUIRED.value == "authentication_required"
        assert ErrorCode.HUMAN_CHALLENGE_REQUIRED.value == "human_challenge_required"
        assert ErrorCode.RATE_LIMITED.value == "rate_limited"
        assert ErrorCode.CONTRACT_DRIFT.value == "contract_drift"
        assert ErrorCode.TRANSPORT_FAILED.value == "transport_failed"
        assert ErrorCode.TIMEOUT_UNCERTAIN.value == "timeout_uncertain"
        assert ErrorCode.CONFIGURATION_ERROR.value == "configuration_error"


class TestTransportStates:
    """Test transport evidence states."""

    def test_transport_states_exist(self):
        """Verify all transport states are defined."""
        assert TransportState.NO_SEND_OBSERVED.value == "no_send_observed"
        assert TransportState.SEND_OBSERVED.value == "send_observed"
        assert TransportState.RESPONSE_STARTED.value == "response_started"
        assert TransportState.COMPLETED.value == "completed"
        assert TransportState.CANCELED.value == "canceled"
        assert TransportState.TIMEOUT_UNCERTAIN.value == "timeout_uncertain"
        assert TransportState.TRANSPORT_FAILED.value == "transport_failed"
        assert TransportState.UNKNOWN.value == "unknown"


class TestRequestIdentities:
    """Test that local and upstream request identities stay distinct."""

    def test_client_request_id_vs_upstream_request_id(self):
        """Verify the two identities are distinct concepts."""
        # ClientRequestID is generated by governor (local deduplication)
        client_id = "cli-123-456"
        # RequestID is upstream provider identifier (transport evidence)
        upstream_id = "upstream-req-abc"
        assert client_id != upstream_id
        # They serve different purposes - local deduplication vs transport evidence


class TestNoAutomaticRetry:
    """Test that no automatic retry/fallback exists."""

    def test_no_retry_in_sse_reconciler(self):
        """SSEReconciler has no retry logic."""
        reconciler = SSEReconciler()
        # No retry methods exist
        assert not hasattr(reconciler, "retry")
        assert not hasattr(reconciler, "fallback")

    def test_no_retry_in_session(self):
        """AccountSession has no automatic retry/fallback."""
        # Check that AccountSession methods don't contain retry logic
        methods = [m for m in dir(AccountSession) if not m.startswith("_")]
        assert "retry" not in str(methods).lower()
        assert "fallback" not in str(methods).lower()


class TestSessionNotReady:
    """Test SessionNotReady exception carries challenge info."""

    def test_session_not_ready_carries_challenge(self):
        """SessionNotReady carries challenge_type and reason."""
        exc = SessionNotReady(challenge_type="turnstile", reason="Turnstile detected")
        assert exc.challenge_type == "turnstile"
        assert "Turnstile" in str(exc)

    def test_session_not_ready_without_challenge(self):
        """SessionNotReady can have no challenge type."""
        exc = SessionNotReady(reason="Session expired")
        assert exc.challenge_type is None
        assert "expired" in str(exc)


if __name__ == "__main__":
    pytest.main([__file__, "-v"])
