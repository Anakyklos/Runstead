// Structural sanitization for anything destined for evidence (stdout, JSON,
// files). No value-specific rules (no usernames, no socket names): only
// structural transformations that hold for any environment:
//   - hex tokens of 12+ chars (UUIDs, conversation ids, session ids) are
//     truncated to 8 chars;
//   - long opaque alphanumeric tokens (24+ chars) are truncated to 12;
//   - any absolute path under the current user's home (or any /home/<user>/)
//     is rewritten to `~`;
//   - URLs are reduced to host (hostname) + path (pathname only, query and
//     fragment ALWAYS dropped) so no raw URL, query string or fragment is
//     ever persisted;
//   - conversation ids are NEVER persisted, not even truncated: evidence
//     carries a non-correlatable placeholder only.
// Applied at the moment a record is created, so both persisted artifacts and
// every printed line are sanitized before they can be captured as evidence.

import os from "node:os";

const HEX_RE = /\b[0-9a-fA-F]{12,}\b/g;
const TOKEN_RE = /\b[A-Za-z0-9._-]{24,}\b/g;

export function redact(value) {
  if (Array.isArray(value)) return value.map(redact);
  if (value && typeof value === "object") {
    // Keys are structural identifiers (classification vocabulary, field
    // names) and must never be truncated; only values are sanitized.
    const out = {};
    for (const [k, v] of Object.entries(value)) out[k] = redact(v);
    return out;
  }
  if (typeof value === "string") {
    let s = value;
    s = s.replace(HEX_RE, (m) => m.slice(0, 8) + "\u2026");
    // Long-token truncation only for strings that look opaque (contain an
    // uppercase letter or a digit). All-lowercase structural vocabulary
    // (classification labels, paths) passes through untouched.
    s = s.replace(TOKEN_RE, (m) =>
      /[A-Z0-9]/.test(m) ? m.slice(0, 12) + "\u2026" : m
    );
    const home = os.homedir();
    if (home && home !== "~") s = s.split(home).join("~");
    s = s.replace(/^\/home\/[^/]+(?=\/|$)/, "~");
    return s;
  }
  return value;
}

// Non-correlatable placeholder for a conversation id. The real id (even a
// truncated fragment) is never persisted; evidence only records WHICH turn
// produced a conversation and that it was redacted.
export function conversationIdEvidence(label) {
  return {
    conversation_id: null,
    conversation_id_placeholder: `conv#${label}`,
    conversation_id_redacted: true,
  };
}

// URL shape for evidence: host = hostname only, path = pathname only
// (query string and fragment ALWAYS dropped). Long tokens in the path are
// subject to the generic redactor, and paths above a bounding length are
// truncated. Never returns the raw URL.
export function urlShape(rawUrl) {
  try {
    const u = new URL(rawUrl);
    return { host: u.hostname, path: truncatePath(redact(u.pathname)) };
  } catch {
    return { host: "", path: "" };
  }
}

// Coarse target shape for Target.targetCreated events: host + FIRST path
// segment only ("c", "about", "auth", ...). This intentionally discards
// everything after the first segment so conversation ids, query strings and
// fragments can never leak through target events. This is the ONLY shape
// used for target URLs; raw target URLs are never logged.
export function targetShape(rawUrl) {
  try {
    const u = new URL(rawUrl);
    if (u.protocol === "file:") {
      // Local fixtures: never expose the local filesystem path, even coarsely.
      return { host: "file:", pathClass: "/" };
    }
    const seg = u.pathname.split("/").filter(Boolean)[0] || "";
    return { host: u.hostname, pathClass: seg ? `/${seg}` : "/" };
  } catch {
    return { host: "", pathClass: "/" };
  }
}

// Long script/asset paths are truncated for evidence compactness.
function truncatePath(path) {
  return path.length > 160 ? path.slice(0, 160) + "\u2026" : path;
}