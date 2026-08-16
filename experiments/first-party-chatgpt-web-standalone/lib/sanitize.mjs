// Structural sanitization for anything destined for evidence (stdout, JSON,
// files). No value-specific rules (no usernames, no socket names): only
// structural transformations that hold for any environment:
//   - hex tokens of 12+ chars (UUIDs, conversation ids, session ids) are
//     truncated to 8 chars;
//   - long opaque alphanumeric tokens (24+ chars) are truncated to 12;
//   - any absolute path under the current user's home (or any /home/<user>/)
//     is rewritten to `~`.
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
