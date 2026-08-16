// harness.js - fail-closed page contract checks for the ChatGPT Web spike.
//
// Evaluated via `orca eval` in the dedicated Orca embedded browser tab. The
// orchestrator calls the functions below and NEVER interacts with the page
// unless the contract check returns ready=true. Unknown states must fail
// closed: no clicks, no typing, no dispatch.

(function () {
  // ChatGPT Web composer labels are localized; probe the known variants.
  var COMPOSER_SELECTORS = [
    '[contenteditable=true][aria-label="Chat with ChatGPT"]', // en
    '[contenteditable=true][aria-label="Converse com o ChatGPT"]', // pt-BR
    '#prompt-textarea', // fallback id used by current web builds
  ];

  function composer() {
    for (var i = 0; i < COMPOSER_SELECTORS.length; i++) {
      var el = document.querySelector(COMPOSER_SELECTORS[i]);
      if (el) return el;
    }
    return null;
  }

  function sendButton() {
    return (
      document.querySelector('button[data-testid="composer-send-button"]') ||
      document.querySelector('button[data-testid="send-button"]') ||
      document.querySelector("#composer-submit-button") ||
      null
    );
  }

  function stopButton() {
    return (
      document.querySelector('button[data-testid="stop-button"]') ||
      Array.from(document.querySelectorAll("button")).find(function (b) {
        return /stop (generating|streaming|response)/i.test(
          b.getAttribute("aria-label") || ""
        );
      }) ||
      null
    );
  }

  // Fingerprint the composer content exactly like a bounded transport would
  // (UTF-16 length + FNV-1a), so the orchestrator can prove the full prompt
  // reached the page without reading it back in plaintext.
  function composerFingerprint() {
    var el = composer();
    if (!el) return { present: false };
    var text = el.children.length
      ? Array.from(el.children)
          .map(function (c) {
            return c.textContent || "";
          })
          .join("\n")
      : el.innerText || "";
    var hash = 2166136261;
    var len = 0;
    for (var i = 0; i < text.length; i++) {
      var code = text.charCodeAt(i);
      hash = Math.imul(hash ^ code, 16777619);
      len++;
    }
    return { present: true, length: len, hash: hash >>> 0 };
  }

  function conversationIdFromUrl() {
    var m = location.pathname.match(/^\/c\/([a-zA-Z0-9-]+)/);
    return m ? m[1] : null;
  }

  // Pure classification matrix, separated from DOM reads so the fail-closed
  // decision can be unit-tested without touching a live page.
  function classify(probe) {
    if (!probe.onChatGPT) return "contract_missing"; // wrong origin: never interact
    if (probe.signedOut) return "login_required"; // session expired / logged out
    if (probe.dialogPresent) return "dialog_blocking"; // unknown modal: never auto-confirm
    if (!probe.composer) return "contract_missing"; // composer element unknown
    return "ready";
  }

  // Full readiness contract. Returns a verdict; only "ready" permits input.
  function ready() {
    var url = location.href;
    var signedOut = Array.from(document.querySelectorAll("button,a")).some(function (e) {
      return /^(log in|sign up)$/i.test((e.innerText || "").trim());
    });
    var dialog = document.querySelector('[role="dialog"]');
    var status = {
      verdict: "unknown",
      url: url,
      onChatGPT: url.indexOf("chatgpt.com") !== -1,
      composer: !!composer(),
      signedOut: signedOut,
      dialogPresent: !!dialog,
      dialogText: dialog ? (dialog.innerText || "").slice(0, 80) : "",
      modelPill: "",
      conversationId: conversationIdFromUrl(),
    };
    var pill = document.querySelector("button.__composer-pill");
    if (pill) status.modelPill = (pill.innerText || "").trim().slice(0, 40);
    status.verdict = classify(status);
    return status;
  }

  // Read the last assistant turn state for final-response detection.
  function turnState() {
    var sections = Array.from(
      document.querySelectorAll('section[data-turn="assistant"]')
    );
    var section = sections[sections.length - 1] || null;
    var messages = section
      ? Array.from(section.querySelectorAll('[data-message-author-role="assistant"]'))
      : [];
    var visible = messages.filter(function (e) {
      var r = e.getBoundingClientRect();
      return r.width > 0 && r.height > 0;
    });
    var message = visible[visible.length - 1] || messages[messages.length - 1] || null;
    var text = message ? message.innerText : "";
    var busy = !!stopButton();
    var terminal =
      !!section &&
      !busy &&
      !!section.querySelector(
        '[data-testid="copy-turn-action-button"], button[aria-label="Copy"], button[aria-label*="Good response"], button[aria-label*="Bad response"], button[aria-label*="Boa resposta"], button[aria-label*="Ruim"]'
      );
    return {
      textLength: text.length,
      textHash: (function () {
        var h = 2166136261;
        for (var i = 0; i < text.length; i++) {
          h = Math.imul(h ^ text.charCodeAt(i), 16777619);
        }
        return h >>> 0;
      })(),
      exactText: text, // bounded token response only; orchestrator validates
      busy: busy,
      terminal: terminal,
      conversationId: conversationIdFromUrl(),
      messageCount: messages.length,
    };
  }

  window.__rsHarness = {
    classify: classify,
    ready: ready,
    turnState: turnState,
    composerFingerprint: composerFingerprint,
    conversationIdFromUrl: conversationIdFromUrl,
    sendButton: function () {
      return !!sendButton();
    },
    clickSend: function () {
      var b = sendButton();
      if (!b || b.disabled) return { clicked: false, reason: "send button missing/disabled" };
      b.click();
      return { clicked: true };
    },
    clickStop: function () {
      var b = stopButton();
      if (!b) return { clicked: false, reason: "no stop button" };
      b.click();
      return { clicked: true };
    },
    // Atomic detect+stop in one eval. Fast generations can outrun a separate
    // click round-trip, so the stop decision must share the DOM read.
    stopIfGenerating: function () {
      var b = stopButton();
      if (!b) return { clicked: false, reason: "no stop button" };
      b.click();
      return { clicked: true, reason: "stop button found and clicked" };
    },
    clearComposer: function () {
      var el = composer();
      if (!el) return { cleared: false };
      if (el.isContentEditable) {
        el.focus();
        document.execCommand("selectAll", false, null);
        document.execCommand("delete", false, null);
      } else {
        el.value = "";
        el.dispatchEvent(new Event("input", { bubbles: true }));
      }
      return { cleared: true };
    },
    focusComposer: function () {
      var el = composer();
      if (!el) return { focused: false };
      el.focus();
      return { focused: true };
    },
  };

  return { harness: "installed", verdict: ready().verdict };
})();
