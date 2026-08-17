# ChatGPT Web Sidecar — JSON-RPC stdio entry point

import asyncio
import json
import sys
import traceback
from typing import Any

from chatgptweb.config import settings
from chatgptweb.session import SessionManager


class JSONRPCServer:
    """JSON-RPC 2.0 server over stdio."""

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
        """Handle completion request."""
        if not self.initialized:
            raise Exception("Not initialized")

        client_request_id = params.get("client_request_id")
        model = params.get("model")
        messages = params.get("messages", [])
        stream = params.get("stream", True)

        if not client_request_id or not model:
            raise Exception("Missing required params: client_request_id, model")

        account_id = params.get("account_id") or settings.default_account
        if not account_id:
            raise Exception("No account_id provided and no default_account configured")

        session = self.session_manager.get_session(account_id)

        # Ensure session is warm
        if not await session.warm_session():
            raise Exception(f"Failed to warm session for account {account_id}")

        # Stream deltas
        async for chunk in session.complete(
            client_request_id=client_request_id,
            model=model,
            messages=messages,
            stream=stream,
        ):
            if chunk.get("done"):
                return {
                    "content": chunk.get("content", ""),
                    "metadata": {
                        "status_code": 200,
                        "request_id": client_request_id,
                        "session_id": session._cf_clearance or "unknown",
                        "duration_ms": 0,  # TODO: measure actual duration
                        "model": model,
                    },
                }
            else:
                # Send streaming delta notification
                self._send_notification("stream_delta", chunk)

        # Should not reach here
        return {"content": "", "metadata": {"status_code": 500}}

    async def handle_health_check(self, params: dict) -> dict:
        """Health check: validate session and model."""
        account_id = params.get("account_id") or settings.default_account
        if not account_id:
            return {"healthy": False, "reason": "no_account"}

        session = self.session_manager.get_session(account_id)
        warm = await session.warm_session()

        if not warm:
            return {"healthy": False, "reason": "session_warm_failed"}

        # Probe drift
        drifted, drift_msg = await session.probe_drift()
        if drifted:
            return {"healthy": False, "reason": "drift_detected", "detail": drift_msg}

        return {"healthy": True, "reason": "ok"}

    async def handle_models(self, params: dict) -> dict:
        """List available models."""
        # ChatGPT Web models (would need dynamic discovery in production)
        return {
            "models": [
                {"id": "gpt-5.6-luna", "display_name": "GPT-5.6 Luna", "context_window": 128000, "capabilities": ["text"]},
                {"id": "gpt-5", "display_name": "GPT-5", "context_window": 128000, "capabilities": ["text", "reasoning"]},
            ]
        }

    async def handle_warm_session(self, params: dict) -> dict:
        """Force session warm."""
        account_id = params.get("account_id") or settings.default_account
        if not account_id:
            raise Exception("No account_id provided")

        session = self.session_manager.get_session(account_id)
        warm = await session.warm_session()
        return {"warmed": warm}

    def _send_notification(self, method: str, params: dict):
        """Send JSON-RPC notification (no id, no response expected)."""
        notification = {"jsonrpc": "2.0", "method": method, "params": params}
        sys.stdout.write(json.dumps(notification) + "\n")
        sys.stdout.flush()

    async def handle_request(self, line: str):
        """Handle incoming JSON-RPC request."""
        try:
            request = json.loads(line)
        except json.JSONDecodeError as e:
            self._send_error(None, -32700, f"Parse error: {e}")
            return

        if not isinstance(request, dict):
            self._send_error(None, -32600, "Invalid request")
            return

        method = request.get("method")
        params = request.get("params", {})
        request_id = request.get("id")

        if method not in self.methods:
            self._send_error(request_id, -32601, f"Method not found: {method}")
            return

        try:
            result = await self.methods[method](params)
            if request_id is not None:
                self._send_response(request_id, result)
        except Exception as e:
            self._send_error(request_id, -32000, str(e))

    def _send_response(self, request_id: Any, result: Any):
        response = {"jsonrpc": "2.0", "id": request_id, "result": result}
        sys.stdout.write(json.dumps(response) + "\n")
        sys.stdout.flush()

    def _send_error(self, request_id: Any, code: int, message: str):
        error = {"code": code, "message": message}
        response = {"jsonrpc": "2.0", "id": request_id, "error": error}
        sys.stdout.write(json.dumps(response) + "\n")
        sys.stdout.flush()


async def main():
    server = JSONRPCServer()

    # Read stdin line by line
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
    # Check required env
    if not settings.master_key:
        print("ERROR: CHATGPTWEB_MASTER_KEY environment variable required", file=sys.stderr)
        sys.exit(1)

    asyncio.run(main())