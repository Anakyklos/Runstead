// instrument.js - sanitized network observability for the ChatGPT Web spike.
//
// Installed via `orca eval` into the page context of the dedicated Orca
// embedded browser tab BEFORE a turn is submitted. It wraps window.fetch and
// XMLHttpRequest and records ONLY:
//   - sequence number, method, hostname, sanitized path, timestamp, status
//   - conversation_id / message id when the backend response exposes them
//     (identifiers needed to correlate the turn; nothing else from the body)
// It NEVER records: headers, authorization material, cookies, prompt text,
// or response content beyond the two identifier fields above.
//
// The orchestrator sets window.__rsPhase to one of:
//   "armed" | "submitted" | "awaiting" | "cancelled" | "done"
// so entries can be classified before/after dispatch.

(function () {
  // Reinstall support: restore any previously wrapped originals first so the
  // instrumentation can be refreshed on a live page.
  if (window.__rsSpikeInstalled && window.__rsOrigFetch) {
    window.fetch = window.__rsOrigFetch;
    XMLHttpRequest.prototype.open = window.__rsOrigOpen;
    XMLHttpRequest.prototype.send = window.__rsOrigSend;
    delete window.__rsSpikeInstalled;
  }
  if (window.__rsSpikeInstalled) return { installed: true, note: "already installed" };
  var seq = 0;
  var log = [];
  var seenConversations = [];

  function sanitize(url) {
    try {
      var u = new URL(url, location.href);
      // Strip query string: may contain nonce/params we do not need.
      return { hostname: u.hostname, path: u.pathname };
    } catch (e) {
      return { hostname: "", path: String(url).slice(0, 120) };
    }
  }

  function pushEntry(method, url, status, conversationId, dispatchedAt) {
    var s = sanitize(url);
    log.push({
      seq: ++seq,
      method: method,
      hostname: s.hostname,
      path: s.path,
      dispatchedAt: dispatchedAt || null,
      ts: Date.now(),
      status: status,
      phase: window.__rsPhase || "pre",
      conversationId: conversationId || null,
    });
  }

  // Identify the conversation id from a backend response WITHOUT retaining the
  // rest of the body. Scans every NDJSON/JSON line but keeps only the id.
  function extractConversationId(text) {
    try {
      var lines = text.split("\n");
      for (var i = 0; i < lines.length; i++) {
        var line = lines[i].trim();
        if (!line) continue;
        var parsed;
        try {
          parsed = JSON.parse(line);
        } catch (e) {
          continue;
        }
        var conv =
          (parsed.message && parsed.message.conversation_id) ||
          parsed.conversation_id ||
          null;
        if (conv) {
          if (seenConversations.indexOf(conv) === -1) seenConversations.push(conv);
          return conv;
        }
      }
      return null;
    } catch (e) {
      return null;
    }
  }

  var origFetch = window.fetch;
  window.__rsOrigFetch = origFetch;
  window.fetch = function (input, init) {
    var method = ((init && init.method) || "GET").toUpperCase();
    var url = typeof input === "string" ? input : input && input.url ? input.url : String(input);
    var dispatchedAt = Date.now();
    return origFetch.apply(this, arguments).then(
      function (response) {
        var convId = null;
        // Only clone/read bodies that look like ChatGPT backend conversation
        // responses, and only to extract the conversation id.
        try {
          var s = sanitize(url);
          if (
            s.hostname.indexOf("chatgpt.com") !== -1 &&
            /conversation/i.test(s.path)
          ) {
            convId = null;
            response
              .clone()
              .text()
              .then(function (body) {
                var id = extractConversationId(body);
                pushEntry(method, url, response.status, id, dispatchedAt);
              })
              .catch(function () {
                pushEntry(method, url, response.status, null, dispatchedAt);
              });
            return response;
          }
        } catch (e) {
          /* fall through to plain record */
        }
        pushEntry(method, url, response.status, convId, dispatchedAt);
        return response;
      },
      function (err) {
        pushEntry(method, url, 0, null, dispatchedAt);
        throw err;
      }
    );
  };

  var origOpen = XMLHttpRequest.prototype.open;
  var origSend = XMLHttpRequest.prototype.send;
  window.__rsOrigOpen = origOpen;
  window.__rsOrigSend = origSend;
  XMLHttpRequest.prototype.open = function (method, url) {
    this.__rsUrl = url;
    return origOpen.apply(this, arguments);
  };
  XMLHttpRequest.prototype.send = function () {
    var xhr = this;
    var url = xhr.__rsUrl || "";
    var dispatchedAt = Date.now();
    xhr.addEventListener("loadend", function () {
      var convId = null;
      try {
        var s = sanitize(url);
        if (
          s.hostname.indexOf("chatgpt.com") !== -1 &&
          /conversation/i.test(s.path) &&
          xhr.responseType === "" &&
          typeof xhr.responseText === "string"
        ) {
          convId = extractConversationId(xhr.responseText);
        }
      } catch (e) {
        /* ignore */
      }
      pushEntry((xhr.__rsMethod || "GET").toUpperCase(), url, xhr.status, convId, dispatchedAt);
    });
    return origSend.apply(this, arguments);
  };

  window.__rsSpike = {
    installed: true,
    installedAt: Date.now(),
    log: log,
    seenConversations: seenConversations,
    reset: function () {
      log.length = 0;
      seenConversations.length = 0;
      seq = 0;
      window.__rsPhase = "pre";
      return { ok: true, resetAt: Date.now() };
    },
    summary: function () {
      var modelEffect = log.filter(function (e) {
        return (
          e.method === "POST" &&
          e.hostname.indexOf("chatgpt.com") !== -1 &&
          /conversation/i.test(e.path)
        );
      });
      return {
        total: log.length,
        modelEffect: modelEffect.length,
        conversations: seenConversations.slice(),
        entries: log.slice(),
      };
    },
  };

  window.__rsSpikeInstalled = true;
  return { installed: true, installedAt: window.__rsSpike.installedAt };
})();
