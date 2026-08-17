# ChatGPT Web Sidecar — JSON-RPC stdio entry point

import asyncio
import json
import sys
import traceback
from typing import Any

from chatgptweb.config import settings
from chatgptweb.session import (
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
            "health_check": self.handle_health_check,
            "models": self.handle_models,
            "warm_session": self.handle_warm_session,
        }

    async def handle_initialize(self, params: dict) -> dict:
        """Handle initialization with config."""
        self.initialized = True
        return {"status": "initialized", "provider": "chatgptweb"}

    async def handle_complete(self, params: dict) -> dict:
        """Handle completion request. Returns transport evidence, never secrets."""
        if not self.initialized:
            raise JSONRPCError(self.INTERNAL_ERROR, "Not initialized")

        client_request_id = params.get("client_request_id")
        model = params.get("model")
        messages = params.get("messages", [])
        stream = params.get("stream", True)

        if not client_request_id or not model:
            raise JSONRPCError(self.INVALID_PARAMS, "Missing required params: client_request_id, model")

        account_id = params.get("account_id") or settings.default_account
        if not account_id:
            raise JSONRPCError(self.INVALID_PARAMS, "No account_id provided and no default_account configured")

        session = self.session_manager.get_session(account_id)

        # Ensure session is warm - may raise SessionNotReady with challenge
        try:
            await session.warm()
        except SessionNotReady as e:
            raise JSONRPCError(
                self.HUMAN_CHALLENGE_REQUIRED if e.challenge_type else self.AUTHENTICATION_REQUIRED,
                f"Session not ready: {e.reason}",
                {"challenge_type": e.challenge_type, "reason": e.reason}
            )

        # Execute completion with streaming
        content_parts = []
        evidence = {}

        async for chunk in session.complete(
            client_request_id=client_request_id,
            model=model,
            messages=messages,
        ):
            if "delta" in chunk:
                content_parts.append(chunk["delta"])
                if stream:
                    self._send_notification("stream_delta", {
                        "client_request_id": client_request_id,
                        "delta": chunk["delta"],
                        "done": chunk.get("done", False),
                    })
            elif "result" in chunk:
                # Final result with transport evidence
                result = chunk["result"]
                evidence = result.get("evidence", {})
                "".join(content_parts)
            elif "error" in chunk:
                # Transport error
                raise JSONRPCError(
                    self.TRANSPORT_FAILED,
                    chunk["error"].get("message", "Transport error"),
                    chunk["error"]
                )

        # Build response with transport evidence (NO secrets)
        return {
            "content": "".join(content_parts),
            "metadata": {
                "client_request_id": client_request_id,  # Echo back local ID
                "status_code": evidence.get("http_status", 200),
                "request_id": evidence.get("upstream_request_id"),  # May be None
                "session_id": None,  # Never return session IDs/secrets
                "duration_ms": evidence.get("duration_ms", 0),
                "model": params.get("model"),
                "transport_state": evidence.get("state"),
                "send_count": evidence.get("send_count", 0),
                "retry_after": evidence.get("retry_after"),
                "reset_at": evidence.get("reset_at"),
                "challenge_type": evidence.get("challenge_type"),
            }
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
                {"id": "gpt-5.6-luna", "display_name": "GPT-5.6 Luna", "context_window": 128000, "capabilities": ["text"]},
                {"id": "gpt-5", "display_name": "GPT-5", "context_window": 128000, "capabilities": ["text", "reasoning"]},
            ]
        }

    async def handle_warm_session(self, params: dict) -> dict:
        """Warm session - may return challenge requirement."""
        account_id = params.get("account_id") or settings.default_account
        if not account_id:
            raise JSONRPCError(self.INVALID_PARAMS, "No account_id provided")

        session = self.session_manager.get_session(account_id)
        try:
            warm, challenge = await session.warm()
            return {"warmed": warm, "challenge_type": challenge}
        except SessionNotReady as e:
            raise JSONRPCError(
                self.HUMAN_CHALLENGE_REQUIRED if e.challenge_type else self.AUTHENTICATION_REQUIRED,
                f"Session not ready: {e.reason}",
                {"challenge_type": e.challenge_type, "reason": e.reason}
            )

    def _send_notification(self, method: str, params: dict):
        """Send JSON-RPC notification (no id, no response expected)."""
        notification = {"jsonrpc": "2.0", "method": method, "params": params}
        sys.stdout.write(json.dumps(notification, separators=(",", ":")) + "\n")
        sys.stdout.flush()

    async def handle_request(self, line: str):
        """Handle incoming JSON-RPC request."""
        try:
            request = json.loads(line)
        except json.JSONDecodeError as e:
            self._send_error(None, self.PARSE_ERROR, f"Parse error: {e}")
            return

        if not isinstance(request, dict):
            self._send_error(None, self.INVALID_REQUEST, "Invalid request")
            return

        method = request.get("method")
        params = request.get("params", {})
        request_id = request.get("id")

        if method not in self.methods:
            self._send_error(request_id, self.METHOD_NOT_FOUND, f"Method not found: {method}")
            return

        try:
            result = await self.methods[method](params)
            if request_id is not None:
                self._send_response(request_id, result)
        except JSONRPCError as e:
            self._send_error(request_id, e.code, e.message, e.data)
        except Exception as e:
            # Log to stderr, not stdout
            traceback.print_exc(file=sys.stderr)
            self._send_error(request_id, self.INTERNAL_ERROR, f"Internal error: {e}")

    def _send_response(self, request_id: Any, result: Any):
        response = {"jsonrpc": "2.0", "id": request_id, "result": result}
        sys.stdout.write(json.dumps(response, separators=(",", ":")) + "\n")
        sys.stdout.flush()

    def _send_error(self, request_id: Any, code: int, message: str, data: dict | None = None):
        error = {"code": code, "message": message}
        if data:
            error["data"] = data
        response = {"jsonrpc": "2.0", "id": request_id, "error": error}
        sys.stdout.write(json.dumps(response, separators=(",", ":")) + "\n")
        sys.stdout.flush()


async def main():
    # Check required env
    if not settings.master_key:
        print("ERROR: CHATGPTWEB_MASTER_KEY environment variable required", file=sys.stderr)
        sys.exit(1)

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
        await server.session_manager.close_all()


if __name__ == "__main__":
    import json
    import sys
    import traceback
    from typing import Any

    asyncio.run(main())
