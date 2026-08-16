// DOM-level page contract for the standalone spike. Reuses the concepts of
// the first spike's harness.js (fail-closed classification, composer
// fingerprint, turn-state polling) but is evaluated directly through CDP
// Runtime.evaluate on the page session owned by this spike.
//
// Rules:
//   - the orchestrator NEVER interacts with the page unless ready() == "ready";
//   - unknown states fail closed (no clicks, no typing, no dispatch);
//   - page code never touches cookies, localStorage, sessionStorage or any
//     credential material; only DOM probes and input insertion.

const INSTALL = `
(function () {
  var COMPOSER_SELECTORS = [
    '[contenteditable=true][aria-label="Chat with ChatGPT"]',
    '[contenteditable=true][aria-label="Converse com o ChatGPT"]',
    '#prompt-textarea'
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
      document.querySelector('#composer-submit-button') ||
      null
    );
  }
  function stopButton() {
    return (
      document.querySelector('button[data-testid="stop-button"]') ||
      Array.from(document.querySelectorAll('button')).find(function (b) {
        return /stop (generating|streaming|response)/i.test(b.getAttribute('aria-label') || '');
      }) ||
      null
    );
  }
  function fnv(str) {
    var h = 2166136261;
    for (var i = 0; i < str.length; i++) {
      h = Math.imul(h ^ str.charCodeAt(i), 16777619);
    }
    return h >>> 0;
  }
  function conversationIdFromUrl() {
    var m = location.pathname.match(/^\\/c\\/([a-zA-Z0-9-]+)/);
    return m ? m[1] : null;
  }
  function ready() {
    var url = location.href.split('?')[0];
    var signedOut = Array.from(document.querySelectorAll('button,a')).some(function (e) {
      var t = (e.innerText || '').trim();
      return (
        /^(log in|sign up)$/i.test(t) ||
        /^(entrar|criar conta|cadastre-se)$/i.test(t)
      );
    });
    var dialog = document.querySelector('[role="dialog"]');
    var comp = composer();
    var authOrigin =
      url.indexOf('https://auth0.openai.com') === 0 ||
      url.indexOf('https://auth.openai.com') === 0 ||
      url.indexOf('https://platform.openai.com') === 0;
    var verdict;
    if (url.indexOf('https://chatgpt.com') !== 0) {
      verdict = authOrigin ? 'auth_pending' : 'contract_missing';
    } else if (signedOut) verdict = 'login_required';
    else if (dialog) verdict = 'dialog_blocking';
    else if (!comp) verdict = 'contract_missing';
    else verdict = 'ready';
    return {
      verdict: verdict,
      url: url,
      authOrigin: authOrigin,
      onChatGPT: url.indexOf('https://chatgpt.com') === 0,
      signedOut: signedOut,
      dialogPresent: !!dialog,
      composerPresent: !!comp,
      conversationId: conversationIdFromUrl()
    };
  }
  function composerFingerprint() {
    var el = composer();
    if (!el) return { present: false };
    var text = el.children.length
      ? Array.from(el.children).map(function (c) { return c.textContent || ''; }).join('\\n')
      : el.innerText || '';
    return { present: true, length: text.length, hash: fnv(text) };
  }
  function focusComposer() {
    var el = composer();
    if (!el) return { focused: false };
    el.focus();
    return { focused: true };
  }
  function clearComposer() {
    var el = composer();
    if (!el) return { cleared: false };
    if (el.isContentEditable) {
      el.focus();
      document.execCommand('selectAll', false, null);
      document.execCommand('delete', false, null);
    } else {
      el.value = '';
      el.dispatchEvent(new Event('input', { bubbles: true }));
    }
    return { cleared: true };
  }
  function clickSend() {
    var b = sendButton();
    if (!b || b.disabled) return { clicked: false, reason: 'send button missing/disabled' };
    b.click();
    return { clicked: true };
  }
  function stopIfGenerating() {
    var b = stopButton();
    if (!b) return { clicked: false, reason: 'no stop button' };
    b.click();
    return { clicked: true, reason: 'stop button found and clicked' };
  }
  function turnState() {
    var sections = Array.from(document.querySelectorAll('section[data-turn="assistant"]'));
    var section = sections[sections.length - 1] || null;
    var messages = section
      ? Array.from(section.querySelectorAll('[data-message-author-role="assistant"]'))
      : [];
    var visible = messages.filter(function (e) {
      var r = e.getBoundingClientRect();
      return r.width > 0 && r.height > 0;
    });
    var message = visible[visible.length - 1] || messages[messages.length - 1] || null;
    var text = message ? message.innerText : '';
    var busy = !!stopButton();
    var terminal = !!section && !busy && !!section.querySelector(
      '[data-testid="copy-turn-action-button"], button[aria-label="Copy"], button[aria-label*="Good response"], button[aria-label*="Bad response"], button[aria-label*="Boa resposta"], button[aria-label*="Ruim"]'
    );
    return {
      textLength: text.length,
      textHash: fnv(text),
      busy: busy,
      terminal: terminal,
      conversationId: conversationIdFromUrl(),
      messageCount: messages.length
    };
  }
  function containsToken(token) {
    var sections = Array.from(document.querySelectorAll('section[data-turn="assistant"]'));
    var section = sections[sections.length - 1] || null;
    var messages = section
      ? Array.from(section.querySelectorAll('[data-message-author-role="assistant"]'))
      : [];
    var text = messages.length ? messages[messages.length - 1].innerText : '';
    return { tokenPresent: text.indexOf(token) !== -1, textLength: text.length };
  }
  window.__rs2 = {
    ready: ready,
    composerFingerprint: composerFingerprint,
    focusComposer: focusComposer,
    clearComposer: clearComposer,
    clickSend: clickSend,
    stopIfGenerating: stopIfGenerating,
    turnState: turnState,
    containsToken: containsToken
  };
  return { harness: 'installed', verdict: ready().verdict };
})()
`;

// Self-contained readiness probe (no install needed): survives navigations,
// so the orchestrator can poll it while the page is still loading.
export const READY_EXPR = `(function () {
  var url = location.href.split('?')[0];
  var signedOut = Array.from(document.querySelectorAll('button,a')).some(function (e) {
    var t = (e.innerText || '').trim();
    return (
      /^(log in|sign up)$/i.test(t) ||
      /^(entrar|criar conta|cadastre-se)$/i.test(t)
    );
  });
  var dialog = document.querySelector('[role="dialog"]');
  var authOrigin =
    url.indexOf('https://auth0.openai.com') === 0 ||
    url.indexOf('https://auth.openai.com') === 0 ||
    url.indexOf('https://platform.openai.com') === 0;
  var comp = null;
  for (var i = 0; i < 3; i++) {
    var sel = ['[contenteditable=true][aria-label="Chat with ChatGPT"]',
               '[contenteditable=true][aria-label="Converse com o ChatGPT"]',
               '#prompt-textarea'][i];
    var el = document.querySelector(sel);
    if (el) { comp = el; break; }
  }
  var verdict;
  if (url.indexOf('https://chatgpt.com') !== 0) {
    verdict = authOrigin ? 'auth_pending' : 'contract_missing';
  } else if (signedOut) verdict = 'login_required';
  else if (dialog) verdict = 'dialog_blocking';
  else if (!comp) verdict = 'contract_missing';
  else verdict = 'ready';
  return {
    verdict: verdict,
    url: url,
    authOrigin: authOrigin,
    onChatGPT: url.indexOf('https://chatgpt.com') === 0,
    signedOut: signedOut,
    dialogPresent: !!dialog,
    composerPresent: !!comp,
    conversationId: (function () {
      var m = location.pathname.match(/^\\/c\\/([a-zA-Z0-9-]+)/);
      return m ? m[1] : null;
    })()
  };
})()
`;

export const INSTALL_EXPR = INSTALL;
export const EXPR = {
  ready: "window.__rs2 ? window.__rs2.ready() : { verdict: 'contract_missing' }",
  composerFingerprint:
    "window.__rs2 ? window.__rs2.composerFingerprint() : { present: false }",
  focusComposer: "window.__rs2.focusComposer()",
  clearComposer: "window.__rs2.clearComposer()",
  clickSend: "window.__rs2.clickSend()",
  stopIfGenerating: "window.__rs2.stopIfGenerating()",
  turnState: "window.__rs2.turnState()",
  containsToken:
    'window.__rs2.containsToken("RUNSTEAD_STANDALONE_OK")',
};
