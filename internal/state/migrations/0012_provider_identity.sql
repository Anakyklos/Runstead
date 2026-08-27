-- Runstead persistence schema, migration 2 (#14).
--
-- Provider-neutral execution identity on every governed provider attempt.
-- The provider attempt projection gains the sanitized, non-secret identity
-- of the configured endpoint that produced it:
--   protocol_family: the compatibility wire family (openai_compatible,
--                    anthropic_compatible, google_compatible);
--   config_identity: the deterministic sanitized configuration identity
--                    (provider.Config.Sanitized, #79) - never option
--                    values, never secret material;
--   request_id:      the upstream request identifier ONLY when actually
--                    observed, and only in its adapter-sanitized (hashed)
--                    form. Missing/unknown stays empty; it is never
--                    guessed.
--
-- These columns are additive and default to empty strings so existing
-- scripted/OmniRoute attempts keep their exact persisted projection.
ALTER TABLE provider_attempts ADD COLUMN protocol_family TEXT NOT NULL DEFAULT '';
ALTER TABLE provider_attempts ADD COLUMN config_identity TEXT NOT NULL DEFAULT '';
ALTER TABLE provider_attempts ADD COLUMN request_id TEXT NOT NULL DEFAULT '';
