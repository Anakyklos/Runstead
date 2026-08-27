-- Runstead persistence schema, migration 13 (#91).
--
-- Durable provider/model operational profiles with evidence provenance.
-- One row per (profile, field): profile_key is the deterministic hash of
-- (sanitized config identity + exact model + protocol family); the plain
-- identity columns are stored alongside so inspection is readable and every
-- row can be re-validated against its key (corruption fails closed).
--
-- The table stores ONLY sanitized operational metadata:
--   config_identity: provider.Config.Sanitized (never option values, never
--                    credentials);
--   evidence_ref:    a SANITIZED evidence reference when the provenance is
--                    observed/authoritative - never a copy of private
--                    evidence content;
--   provenance:      unknown|configured|observed|authoritative semantics;
--   value:           the effective bound/envelope value (0 or absent =
--                    unknown, never guessed);
--   profile_version: the operational profile schema version ("v1").
--
-- No prompts, no response bodies, no headers, no credentials ever enter
-- this table.
CREATE TABLE provider_operational_profiles (
    profile_key     TEXT NOT NULL,
    provider_id     TEXT NOT NULL,
    protocol_family TEXT NOT NULL,
    config_identity TEXT NOT NULL,
    model           TEXT NOT NULL,
    profile_version TEXT NOT NULL,
    field           TEXT NOT NULL,
    value           INTEGER NOT NULL,
    provenance      TEXT NOT NULL,
    evidence_ref    TEXT NOT NULL DEFAULT '',
    updated_at      TEXT NOT NULL,
    created_at      TEXT NOT NULL,
    PRIMARY KEY (profile_key, field)
);
