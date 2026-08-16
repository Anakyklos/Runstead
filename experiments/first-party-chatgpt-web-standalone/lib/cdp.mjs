// Minimal CDP (Chrome DevTools Protocol) client built only on Node's built-in
// WebSocket (node >= 22). No npm dependencies. Supports browser-level
// connection plus flattened target sessions, which is all this spike needs.
//
// The browser is the only CDP server; this module only:
//   - connects to the browser websocket endpoint;
//   - correlates command ids with responses;
//   - routes session events to registered handlers.

export class CdpError extends Error {
  constructor(code, message) {
    super(message);
    this.code = code;
  }
}

export class Cdp {
  constructor(wsUrl) {
    this.wsUrl = wsUrl;
    this.ws = null;
    this.nextId = 1;
    this.pending = new Map();
    this.handlers = new Set();
    this.closedPromise = new Promise((resolve) => {
      this._resolveClosed = resolve;
    });
    this.closed = null; // {code, clean} once closed
  }

  async connect(timeoutMs = 15000) {
    await new Promise((resolve, reject) => {
      let ws;
      try {
        ws = new WebSocket(this.wsUrl);
      } catch (err) {
        reject(new CdpError("cdp_connect_failed", String(err)));
        return;
      }
      this.ws = ws;
      const timer = setTimeout(() => {
        reject(new CdpError("cdp_connect_timeout", "no open within timeout"));
      }, timeoutMs);
      ws.addEventListener(
        "open",
        () => {
          clearTimeout(timer);
          resolve();
        },
        { once: true }
      );
      ws.addEventListener(
        "error",
        () => {
          clearTimeout(timer);
          reject(new CdpError("cdp_connect_failed", "websocket error"));
        },
        { once: true }
      );
    });
    this.ws.addEventListener("message", (ev) => this._onMessage(ev.data));
    this.ws.addEventListener("close", (ev) => this._onClose(ev));
  }

  _onMessage(data) {
    let msg;
    try {
      msg = JSON.parse(data);
    } catch {
      return;
    }
    if (msg.id !== undefined) {
      const p = this.pending.get(msg.id);
      if (!p) return;
      this.pending.delete(msg.id);
      if (msg.error) {
        p.reject(
          new CdpError(
            "cdp_error",
            `${msg.error.code || "unknown"}: ${msg.error.message || ""}`
          )
        );
      } else {
        p.resolve(msg.result ?? {});
      }
      return;
    }
    if (msg.method) {
      for (const fn of this.handlers) {
        try {
          fn(msg.method, msg.params ?? {}, msg.sessionId ?? null);
        } catch {
          // handler errors must not break the connection
        }
      }
    }
  }

  _onClose(ev) {
    for (const [, p] of this.pending) {
      p.reject(new CdpError("cdp_closed", "connection closed"));
    }
    this.pending.clear();
    this.closed = { code: ev.code, clean: ev.wasClean };
    this._resolveClosed(this.closed);
  }

  send(method, params = {}, sessionId = null, timeoutMs = 30000) {
    const id = this.nextId++;
    const msg = { id, method, params };
    if (sessionId) msg.sessionId = sessionId;
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        this.pending.delete(id);
        reject(new CdpError("cdp_timeout", `${method} timed out`));
      }, timeoutMs);
      this.pending.set(id, {
        resolve: (v) => {
          clearTimeout(timer);
          resolve(v);
        },
        reject: (e) => {
          clearTimeout(timer);
          reject(e);
        },
      });
      const deliver = () => {
        if (this.ws && this.ws.readyState === WebSocket.OPEN) {
          this.ws.send(JSON.stringify(msg));
        } else {
          this.pending.delete(id);
          clearTimeout(timer);
          reject(new CdpError("cdp_closed", "connection not open"));
        }
      };
      if (this.ws && this.ws.readyState === WebSocket.OPEN) deliver();
      else this.ws.addEventListener("open", deliver, { once: true });
    });
  }

  onEvent(fn) {
    this.handlers.add(fn);
  }

  close() {
    try {
      if (this.ws) this.ws.close();
    } catch {
      // ignore
    }
  }
}
