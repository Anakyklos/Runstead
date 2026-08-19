# ChatGPT Web Sidecar — JSON-RPC stdio entry point

import asyncio
import json
import sys
import traceback
from typing import Any

from chatgptweb.config import settings
from chatgptweb.session import (
    DriftDetected,
    SessionManager,
    SessionNotReady,
)


class JSONRPCError(Exception):
    """JSON-RPC typed error."""

    def __init__(self, code: int, message: str, data: dict | None = None):
        self.code = code
        self.message = message
        self.data = data or {}
        super().__init__(message)


class JSONRPCServer:
    """JSON-RPC 2.0 server over stdio."""

    # JSON-RPC error codes
    PARSE_ERROR = -32700
    INVALID_REQUEST = -32600
    METHOD_NOT_FOUND = -32601
    INVALID_PARAMS = -32602
    INTERNAL_ERROR = -32603
    # Application errors
    AUTHENTICATION_REQUIRED = -32001
    HUMAN_CHALLENGE_REQUIRED = -32002
    RATE_LIMITED = -32003
    CONTRACT_DRIFT = -32004
    TRANSPORT_FAILED = -32005
    TIMEOUT_UNCERTAIN = -32006
    CONFIGURATION_ERROR = -32007

    def __init__(self):
        self.session_manager = SessionManager()
        self.initialized = False
        self.methods = {
            "initialize": self.handle_initialize,
            "complete": self.handle_complete,
            "cancel": self.handle_cancel,
            "health_check": self.handle_health_check,
            "models": self.handle_models,
            "warm_session": self.handle_warm_session,
        }
        self._active_completions: dict[str, asyncio.Event] = {}
        self._background_tasks: set[asyncio.Task] = set()
        self._output_lock = asyncio.Lock()

    async def handle_initialize(self, params: dict) -> dict:
        """Handle initialization with config."""
        self.initialized = True
        return {"status": "initialized", "provider": "chatgptweb"}

    async def handle_complete(
        self, params: dict, cancel_event: asyncio.Event | None = None
    ) -> dict:
        """Handle completion request. Returns transport evidence, never secrets."""
        if not self.initialized:
            raise JSONRPCError(self.INTERNAL_ERROR, "Not initialized")

        client_request_id = params.get("client_request_id")
        model = params.get("model")
        messages = params.get("messages", [])
        stream = params.get("stream", True)

        if not client_request_id or not model:
            raise JSONRPCError(
                self.INVALID_PARAMS, "Missing required params: client_request_id, model"
            )

        if cancel_event is None:
            if client_request_id in self._active_completions:
                raise JSONRPCError(
                    self.INVALID_PARAMS,
                    f"Completion already in flight: {client_request_id}",
                )
            cancel_event = asyncio.Event()
            self._active_completions[client_request_id] = cancel_event
        elif self._active_completions.get(client_request_id) is not cancel_event:
            self._active_completions[client_request_id] = cancel_event

        account_id = params.get("account_id") or settings.default_account
        if not account_id:
            self._active_completions.pop(client_request_id, None)
            raise JSONRPCError(
                self.INVALID_PARAMS, "No account_id provided and no default_account configured"
            )

        session = self.session_manager.get_session(account_id)

        try:
            # Ensure session is warm - may raise SessionNotReady with challenge.
            try:
                await session.warm()
            except SessionNotReady as e:
                raise JSONRPCError(
                    self.HUMAN_CHALLENGE_REQUIRED
                    if e.challenge_type
                    else self.AUTHENTICATION_REQUIRED,
                    f"Session not ready: {e.reason}",
                    {"challenge_type": e.challenge_type, "reason": e.reason},
                ) from None
            except DriftDetected as e:
                raise JSONRPCError(self.CONTRACT_DRIFT, f"Drift detected: {e.message}") from None

            # Execute completion with streaming. The event is passed through
            # the declared boundary so cancel() can abort the same request.
            content_parts = []
            evidence = None
            try:
                async for chunk in session.complete(
                    client_request_id=client_request_id,
                    model=model,
                    messages=messages,
                    cancel_event=cancel_event,
                ):
                    if "delta" in chunk:
                        content_parts.append(chunk["delta"])
                        if stream:
                            await self._send_notification(
                                "stream_delta",
                                {
                                    "client_request_id": client_request_id,
                                    "delta": chunk["delta"],
                                    "done": chunk.get("done", False),
                                },
                            )
                    elif "result" in chunk:
                        # Final result with transport evidence (already serialized as dict).
                        result = chunk["result"]
                        evidence = result.get("evidence", None)
                    elif "error" in chunk:
                        # Transport error - use jsonrpc_code from evidence for proper mapping.
                        error_data = chunk["error"]
                        jsonrpc_code = error_data.get("jsonrpc_code", self.TRANSPORT_FAILED)
                        raise JSONRPCError(
                            jsonrpc_code,
                            error_data.get("message", "Transport error"),
                            error_data,
                        )
            except DriftDetected as e:
                raise JSONRPCError(self.CONTRACT_DRIFT, f"Drift detected: {e.message}") from None

            # Build response with transport evidence (NO secrets).
            if evidence is None:
                evidence = {}

            return {
                "content": "".join(content_parts),
                "metadata": {
                    "client_request_id": client_request_id,
                    "status_code": evidence.get("http_status"),
                    "request_id": evidence.get("upstream_request_id"),
                    "session_id": None,
                    "duration_ms": evidence.get("duration_ms", 0),
                    "model": params.get("model"),
                    "transport_state": evidence.get("state"),
                    "send_count": evidence.get("send_count", 0),
                    "retry_after": evidence.get("retry_after"),
                    "reset_at": evidence.get("reset_at"),
                    "challenge_type": evidence.get("challenge_type"),
                },
            }
        finally:
            if self._active_completions.get(client_request_id) is cancel_event:
                self._active_completions.pop(client_request_id, None)

    async def handle_cancel(self, params: dict) -> dict:
        """Request cancellation of an in-flight completion by local request ID."""
        client_request_id = params.get("client_request_id")
        if not client_request_id:
            raise JSONRPCError(self.INVALID_PARAMS, "Missing required param: client_request_id")

        cancel_event = self._active_completions.get(client_request_id)
        if cancel_event is None:
            return {
                "requested": False,
                "client_request_id": client_request_id,
                "state": "not_in_flight",
            }

        cancel_event.set()
        return {
            "requested": True,
            "client_request_id": client_request_id,
            "state": "cancellation_requested",
        }

    async def handle_health_check(self, params: dict) -> dict:
        """Health check without model effects."""
        account_id = params.get("account_id") or settings.default_account
        if not account_id:
            return {"healthy": False, "reason": "no_account"}

        session = self.session_manager.get_session(account_id)
        healthy, reason = await session.health_check()
        return {"healthy": healthy, "reason": reason}

    async def handle_models(self, params: dict) -> dict:
        """List available models (static for research)."""
        return {
            "models": [
                {
                    "id": "gpt-5.6-luna",
                    "display_name": "GPT-5.6 Luna",
                    "context_window": 128000,
                    "capabilities": ["text"],
                },
                {
                    "id": "gpt-5",
                    "display_name": "GPT-5",
                    "context_window": 128000,
                    "capabilities": ["text", "reasoning"],
                },
            ]
        }

    async def handle_warm_session(self, params: dict) -> dict:
        """Warm session - may raise SessionNotReady with challenge."""
        account_id = params.get("account_id") or settings.default_account
        if not account_id:
            raise JSONRPCError(self.INVALID_PARAMS, "No account_id provided")

        session = self.session_manager.get_session(account_id)
        try:
            await session.warm()
            return {"warmed": True, "challenge_type": None}
        except SessionNotReady as e:
            raise JSONRPCError(
                self.HUMAN_CHALLENGE_REQUIRED if e.challenge_type else self.AUTHENTICATION_REQUIRED,
                f"Session not ready: {e.reason}",
                {"challenge_type": e.challenge_type, "reason": e.reason},
            ) from None
        except DriftDetected as e:
            raise JSONRPCError(self.CONTRACT_DRIFT, f"Drift detected: {e.message}") from None

    async def _send_notification(self, method: str, params: dict):
        """Send a JSON-RPC notification without interleaving concurrent writes."""
        notification = {"jsonrpc": "2.0", "method": method, "params": params}
        async with self._output_lock:
            sys.stdout.write(json.dumps(notification, separators=(",", ":")) + "\n")
            sys.stdout.flush()

    async def _send_response(self, request_id: Any, result: Any):
        response = {"jsonrpc": "2.0", "id": request_id, "result": result}
        async with self._output_lock:
            sys.stdout.write(json.dumps(response, separators=(",", ":")) + "\n")
            sys.stdout.flush()

    async def _send_error(self, request_id: Any, code: int, message: str, data: dict | None = None):
        error = {"code": code, "message": message}
        if data:
            error["data"] = data
        response = {"jsonrpc": "2.0", "id": request_id, "error": error}
        async with self._output_lock:
            sys.stdout.write(json.dumps(response, separators=(",", ":")) + "\n")
            sys.stdout.flush()

    async def _dispatch_request(
        self, request: dict, completion_event: asyncio.Event | None = None
    ) -> None:
        method = request["method"]
        params = request.get("params", {})
        request_id = request.get("id")
        try:
            if method == "complete":
                result = await self.handle_complete(params, cancel_event=completion_event)
            else:
                result = await self.methods[method](params)
            if request_id is not None:
                await self._send_response(request_id, result)
        except JSONRPCError as e:
            await self._send_error(request_id, e.code, e.message, e.data)
        except Exception as e:
            # Log to stderr, not stdout.
            traceback.print_exc(file=sys.stderr)
            await self._send_error(request_id, self.INTERNAL_ERROR, f"Internal error: {e}")

    async def handle_request(self, line: str):
        """Parse one request and schedule model effects without blocking cancel()."""
        try:
            request = json.loads(line)
        except json.JSONDecodeError as e:
            await self._send_error(None, self.PARSE_ERROR, f"Parse error: {e}")
            return

        if not isinstance(request, dict):
            await self._send_error(None, self.INVALID_REQUEST, "Invalid request")
            return

        method = request.get("method")
        params = request.get("params", {})
        request_id = request.get("id")

        if method not in self.methods:
            await self._send_error(request_id, self.METHOD_NOT_FOUND, f"Method not found: {method}")
            return
        if not isinstance(params, dict):
            await self._send_error(request_id, self.INVALID_PARAMS, "params must be an object")
            return

        if method == "complete":
            client_request_id = params.get("client_request_id")
            completion_event = None
            if client_request_id:
                if client_request_id in self._active_completions:
                    await self._send_error(
                        request_id,
                        self.INVALID_PARAMS,
                        f"Completion already in flight: {client_request_id}",
                    )
                    return
                completion_event = asyncio.Event()
                self._active_completions[client_request_id] = completion_event

            task = asyncio.create_task(self._dispatch_request(request, completion_event))
            self._background_tasks.add(task)
            task.add_done_callback(self._background_tasks.discard)
            return

        await self._dispatch_request(request)

    async def wait_for_background_tasks(self):
        """Wait until all background completions have emitted their responses."""
        while self._background_tasks:
            await asyncio.gather(*tuple(self._background_tasks), return_exceptions=True)


async def main():
    # Credentials remain under the browser profile; no secret is loaded here.
    server = JSONRPCServer()

    loop = asyncio.get_event_loop()
    reader = asyncio.StreamReader()
    protocol = asyncio.StreamReaderProtocol(reader)
    await loop.connect_read_pipe(lambda: protocol, sys.stdin)

    try:
        while True:
            line = await reader.readline()
            if not line:
                break
            line = line.decode().strip()
            if line:
                await server.handle_request(line)
    except KeyboardInterrupt:
        pass
    finally:
        await server.wait_for_background_tasks()
        await server.session_manager.close_all()


if __name__ == "__main__":
    asyncio.run(main())
