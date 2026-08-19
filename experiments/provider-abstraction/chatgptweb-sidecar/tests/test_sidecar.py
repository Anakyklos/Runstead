import asyncio
import json
from pathlib import Path
from unittest.mock import AsyncMock

import pytest

from chatgptweb.__main__ import JSONRPCError, JSONRPCServer
from chatgptweb.config import settings
from chatgptweb.session import (
    AccountSession,
    BrowserSession,
    ChallengeType,
    DriftDetected,
    ErrorCode,
    PhysicalAbortUnproven,
    RequestCanceled,
    SessionNotReady,
    SSEReconciler,
    TransportState,
    _evidence_to_dict,
)


def sse_event(content: str) -> str:
    return json.dumps({"message": {"content": {"parts": [content]}}})


@pytest.fixture
def account_session(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> AccountSession:
    accounts_dir = tmp_path / "accounts"
    monkeypatch.setattr(settings, "accounts_dir", accounts_dir)
    return AccountSession("acct-test", tmp_path / "browser-profile")


class FakePage:
    """Deterministic browser-page fixture for one physical fetch lifecycle."""

    def __init__(self, mode: str = "timeout"):
        self.mode = mode
        self.fetch_starts = 0
        self.abort_calls = 0
        self.cleanup_calls = 0
        self.aborted = False

    async def evaluate(self, script: str, await_promise: bool = True):
        if "const controller = new AbortController()" in script:
            self.fetch_starts += 1
            return {"started": True}
        if "controller.abort()" in script:
            self.abort_calls += 1
            self.aborted = True
            return {"found": True}
        if "globalThis.__runsteadFetchControllers?.delete" in script:
            self.cleanup_calls += 1
            return True
        if "globalThis.__runsteadFetchStates?.get" in script:
            if self.mode == "timeout" and not self.aborted:
                return {
                    "state": "response_started",
                    "status": 200,
                    "headers": {},
                    "text": "",
                    "done": False,
                    "response_started": True,
                    "error_name": None,
                    "error_message": None,
                    "abort_observed": False,
                }
            if self.aborted:
                return {
                    "state": "aborted",
                    "status": 0,
                    "headers": {},
                    "text": "",
                    "done": True,
                    "response_started": False,
                    "error_name": "AbortError",
                    "error_message": "The operation was aborted.",
                    "abort_observed": True,
                }
            return {
                "state": "completed",
                "status": 200,
                "headers": {"content-type": "text/event-stream"},
                "text": "data: [DONE]\n",
                "done": True,
                "response_started": True,
                "error_name": None,
                "error_message": None,
                "abort_observed": False,
            }
        raise AssertionError(f"unexpected browser script: {script[:120]}")


async def collect_completion(
    session: AccountSession,
    messages: list[dict] | None = None,
    **kwargs,
) -> list[dict]:
    return [
        chunk
        async for chunk in session.complete(
            "client-1",
            "model-test",
            messages or [],
            **kwargs,
        )
    ]


class TestSSEReconciler:
    def test_cumulative_content_is_reconciled(self):
        reconciler = SSEReconciler()
        assert reconciler.process_chunk(sse_event("Hello")) == "Hello"
        assert reconciler.process_chunk(sse_event("Hello world")) == " world"
        assert reconciler.process_chunk(sse_event("Hello world")) is None
        assert reconciler.process_chunk(sse_event("Hel")) is None
        assert reconciler.process_chunk("[DONE]") == ""

    @pytest.mark.asyncio
    async def test_sequential_completions_do_not_share_sse_state(self, account_session):
        account_session._drift_gate = AsyncMock()
        responses = iter(["first", "second"])

        async def stream(*args, **kwargs):
            content = next(responses)
            yield {"type": "headers", "status": 200, "headers": {}}
            yield {"type": "line", "data": f"data: {sse_event(content)}"}
            yield {"type": "line", "data": "data: [DONE]"}

        account_session.browser.cdp_fetch_stream = stream
        first = await collect_completion(account_session)
        second = await collect_completion(account_session)
        assert first[-1]["result"]["content"] == "first"
        assert second[-1]["result"]["content"] == "second"

    @pytest.mark.asyncio
    async def test_concurrent_completions_do_not_contaminate_reconciler(self, account_session):
        account_session._drift_gate = AsyncMock()
        started = asyncio.Barrier(2)

        async def stream(*args, **kwargs):
            content = kwargs["json_data"]["messages"][0]["content"]
            await started.wait()
            yield {"type": "headers", "status": 200, "headers": {}}
            yield {"type": "line", "data": f"data: {sse_event(content)}"}
            yield {"type": "line", "data": "data: [DONE]"}

        account_session.browser.cdp_fetch_stream = stream
        first_task = asyncio.create_task(
            collect_completion(account_session, messages=[{"role": "user", "content": "alpha"}])
        )
        second_task = asyncio.create_task(
            collect_completion(account_session, messages=[{"role": "user", "content": "beta"}])
        )
        first, second = await asyncio.gather(first_task, second_task)
        assert first[-1]["result"]["content"] == "alpha"
        assert second[-1]["result"]["content"] == "beta"


class TestDriftGate:
    @pytest.mark.asyncio
    async def test_persisted_baseline_is_not_overwritten_on_drift(self, tmp_path, monkeypatch):
        accounts_dir = tmp_path / "accounts"
        monkeypatch.setattr(settings, "accounts_dir", accounts_dir)
        account_dir = accounts_dir / "acct-test"
        account_dir.mkdir(parents=True)
        baseline_file = account_dir / "sentinel_hash.txt"
        baseline_file.write_text("persisted-old\n")
        session = AccountSession("acct-test", tmp_path / "browser-profile")
        session._probe_drift_hash = AsyncMock(return_value="current-new")

        with pytest.raises(DriftDetected):
            await session._drift_gate()

        assert session._drift_hash == "persisted-old"
        assert baseline_file.read_text() == "persisted-old\n"

    @pytest.mark.asyncio
    async def test_drift_probe_failure_fails_closed(self, account_session):
        account_session._probe_drift_hash = AsyncMock(return_value=None)
        with pytest.raises(DriftDetected, match="probe failed"):
            await account_session._drift_gate()


class TestPhysicalTransportCancellation:
    @pytest.mark.asyncio
    async def test_timeout_aborts_same_physical_post_once(self):
        page = FakePage(mode="timeout")
        browser = BrowserSession("acct-test", Path("/tmp/unused-profile"))
        browser.page = page

        async def consume():
            return [
                item
                async for item in browser.cdp_fetch_stream(
                    "http://127.0.0.1:9/conversation",
                    json_data={"prompt": "fixture"},
                    timeout=0,
                )
            ]

        with pytest.raises(asyncio.TimeoutError):
            await consume()
        assert page.fetch_starts == 1
        assert page.abort_calls == 1
        assert page.cleanup_calls == 1

    @pytest.mark.asyncio
    async def test_unobserved_abort_is_not_reported_as_canceled(self):
        page = FakePage(mode="timeout")
        browser = BrowserSession("acct-test", Path("/tmp/unused-profile"))
        browser.page = page
        browser._abort_and_wait = AsyncMock(return_value=(None, False))

        async def consume():
            return [
                item
                async for item in browser.cdp_fetch_stream(
                    "http://127.0.0.1:9/conversation",
                    json_data={"prompt": "fixture"},
                    timeout=0,
                )
            ]

        with pytest.raises(PhysicalAbortUnproven):
            await consume()
        assert page.fetch_starts == 1
        assert page.abort_calls == 0


class TestCompletionEvidence:
    @pytest.mark.asyncio
    @pytest.mark.parametrize("status", [401, 403])
    async def test_auth_status_maps_to_jsonrpc_32001(self, account_session, status):
        account_session._drift_gate = AsyncMock()

        async def stream(*args, **kwargs):
            yield {"type": "headers", "status": status, "headers": {}}

        account_session.browser.cdp_fetch_stream = stream
        chunks = await collect_completion(account_session)
        assert chunks[-1]["error"]["jsonrpc_code"] == -32001
        assert chunks[-1]["error"]["evidence"]["send_count"] == 1

    @pytest.mark.asyncio
    async def test_rate_limit_maps_to_jsonrpc_32003(self, account_session):
        account_session._drift_gate = AsyncMock()

        async def stream(*args, **kwargs):
            yield {"type": "headers", "status": 429, "headers": {"retry-after": "3"}}

        account_session.browser.cdp_fetch_stream = stream
        chunks = await collect_completion(account_session)
        assert chunks[-1]["error"]["jsonrpc_code"] == -32003
        assert chunks[-1]["error"]["evidence"]["retry_after"] == 3.0

    @pytest.mark.asyncio
    async def test_truncated_sse_is_timeout_uncertain_not_completed(self, account_session):
        account_session._drift_gate = AsyncMock()

        async def stream(*args, **kwargs):
            yield {"type": "headers", "status": 200, "headers": {}}
            yield {"type": "line", "data": f"data: {sse_event('partial')}"}

        account_session.browser.cdp_fetch_stream = stream
        chunks = await collect_completion(account_session)
        assert "result" not in chunks[-1]
        assert chunks[-1]["error"]["jsonrpc_code"] == -32006
        assert chunks[-1]["error"]["evidence"]["state"] == "timeout_uncertain"

    @pytest.mark.asyncio
    async def test_exception_after_headers_is_timeout_uncertain(self, account_session):
        account_session._drift_gate = AsyncMock()

        async def stream(*args, **kwargs):
            yield {"type": "headers", "status": 200, "headers": {}}
            raise RuntimeError("fixture transport stopped")

        account_session.browser.cdp_fetch_stream = stream
        chunks = await collect_completion(account_session)
        assert chunks[-1]["error"]["jsonrpc_code"] == -32006
        assert chunks[-1]["error"]["evidence"]["state"] == "timeout_uncertain"

    @pytest.mark.asyncio
    async def test_exception_before_headers_is_transport_failure(self, account_session):
        account_session._drift_gate = AsyncMock()

        async def stream(*args, **kwargs):
            raise RuntimeError("fixture transport failed")
            yield  # pragma: no cover

        account_session.browser.cdp_fetch_stream = stream
        chunks = await collect_completion(account_session)
        assert chunks[-1]["error"]["jsonrpc_code"] == -32005
        assert chunks[-1]["error"]["evidence"]["send_count"] == 0

    @pytest.mark.asyncio
    async def test_evidence_is_json_serializable(self, account_session):
        account_session._drift_gate = AsyncMock()

        async def stream(*args, **kwargs):
            yield {"type": "headers", "status": 429, "headers": {}}

        account_session.browser.cdp_fetch_stream = stream
        chunks = await collect_completion(account_session)
        json.dumps(chunks[-1]["error"]["evidence"])


class FakeSession:
    def __init__(self, *, completion=None, warm_error=None):
        self.completion = completion
        self.warm_error = warm_error

    async def warm(self):
        if self.warm_error:
            raise self.warm_error

    async def complete(self, **kwargs):
        if isinstance(self.completion, Exception):
            raise self.completion
        if self.completion is not None:
            yield self.completion

    async def health_check(self):
        return True, "ok"


@pytest.mark.asyncio
@pytest.mark.parametrize(
    ("status", "code"),
    [(401, -32001), (403, -32001), (429, -32003), (500, -32005)],
)
async def test_jsonrpc_transport_error_codes(status, code):
    server = JSONRPCServer()
    server.initialized = True
    evidence = {"state": "transport_failed", "http_status": status, "send_count": 1}
    fake = FakeSession(
        completion={
            "error": {
                "message": f"HTTP {status}",
                "evidence": evidence,
                "jsonrpc_code": code,
            }
        }
    )
    server.session_manager.get_session = lambda account_id: fake
    with pytest.raises(JSONRPCError) as exc_info:
        await server.handle_complete(
            {"client_request_id": "client-1", "account_id": "acct", "model": "m", "messages": []}
        )
    assert exc_info.value.code == code
    json.dumps(exc_info.value.data)


@pytest.mark.asyncio
async def test_jsonrpc_challenge_drift_and_timeout_codes():
    cases = [
        (SessionNotReady(challenge_type="captcha", reason="human required"), -32002),
        (DriftDetected("fixture drift"), -32004),
    ]
    for error, expected in cases:
        server = JSONRPCServer()
        server.initialized = True
        server.session_manager.get_session = lambda account_id, error=error: FakeSession(
            warm_error=error
        )
        with pytest.raises(JSONRPCError) as exc_info:
            await server.handle_complete(
                {
                    "client_request_id": "client-1",
                    "account_id": "acct",
                    "model": "m",
                    "messages": [],
                }
            )
        assert exc_info.value.code == expected

    server = JSONRPCServer()
    server.initialized = True
    server.session_manager.get_session = lambda account_id: FakeSession(
        completion={
            "error": {
                "message": "timeout",
                "evidence": {"state": "timeout_uncertain"},
                "jsonrpc_code": -32006,
            }
        }
    )
    with pytest.raises(JSONRPCError) as exc_info:
        await server.handle_complete(
            {"client_request_id": "client-1", "account_id": "acct", "model": "m", "messages": []}
        )
    assert exc_info.value.code == -32006


@pytest.mark.asyncio
async def test_drift_during_completion_maps_to_jsonrpc_32004():
    server = JSONRPCServer()
    server.initialized = True
    fake = FakeSession(completion=DriftDetected("completion drift"))
    server.session_manager.get_session = lambda account_id: fake
    with pytest.raises(JSONRPCError) as exc_info:
        await server.handle_complete(
            {"client_request_id": "client-1", "account_id": "acct", "model": "m", "messages": []}
        )
    assert exc_info.value.code == -32004


@pytest.mark.asyncio
async def test_client_request_id_is_not_upstream_request_id():
    server = JSONRPCServer()
    server.initialized = True
    fake = FakeSession(
        completion={
            "result": {
                "content": "done",
                "evidence": {
                    "http_status": 200,
                    "upstream_request_id": "upstream-123",
                    "duration_ms": 7,
                    "state": "completed",
                    "send_count": 1,
                },
            }
        }
    )
    server.session_manager.get_session = lambda account_id: fake
    result = await server.handle_complete(
        {"client_request_id": "client-123", "account_id": "acct", "model": "m", "messages": []}
    )
    assert result["metadata"]["client_request_id"] == "client-123"
    assert result["metadata"]["request_id"] == "upstream-123"
    assert result["metadata"]["client_request_id"] != result["metadata"]["request_id"]


@pytest.mark.asyncio
async def test_warm_reuses_already_warmed_session_without_relaunching_browser(
    account_session, monkeypatch
):
    monkeypatch.setattr("chatgptweb.session.UC_AVAILABLE", True)
    monkeypatch.setattr("chatgptweb.session.asyncio.sleep", AsyncMock())
    account_session.browser.start = AsyncMock()
    account_session.browser.stop = AsyncMock()
    account_session.browser.navigate_and_wait = AsyncMock(return_value=True)
    account_session.browser.detect_challenge = AsyncMock(return_value=None)
    account_session.browser.has_valid_session = AsyncMock(return_value=True)
    account_session._probe_drift_hash = AsyncMock(return_value="stable")

    await account_session.warm()
    await account_session.warm()

    account_session.browser.start.assert_awaited_once()
    account_session.browser.navigate_and_wait.assert_awaited_once()


def test_types_and_serialization_contracts():
    assert ChallengeType.CAPTCHA.value == "captcha"
    assert ErrorCode.TIMEOUT_UNCERTAIN.value == "timeout_uncertain"
    assert TransportState.COMPLETED.value == "completed"
    assert RequestCanceled.__doc__
    assert _evidence_to_dict(None) == {}
