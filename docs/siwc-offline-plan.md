# Sign in with ChatGPT — offline implementation sequence

Status: **not operational**. Maintainer decision: issue #174, authorized 2026-10-08.
PR 1 scope: issue #175. No ChatGPT login, no inference, no API key substitution.

## Contract boundary

Provider declarations v1 and frozen execution contracts v1 retain their
legacy serialization and identity. A v1 provider declaration refuses
`wire_contract`, `account_binding` and `credential_binding` even if blank.

V2 provider declarations use `version: 2`, a closed `wire_contract` vocabulary
and validated opaque bindings. The SIWC form requires:
- `protocol_family: openai_compatible`;
- `wire_contract: responses_siwc_v1` (different from Chat Completions);
- `base_url: https://api.openai.com/v1`;
- a configured exact `model` and non-secret `auth_ref`;
- `account_binding` and `credential_binding` in
  `hmac-sha256:v1:<64 lowercase hex digits>` format, distinct from each other;
- the existing declared capability profile and safe single-attempt RouteSafety;
- no untyped `options` fields.

These bindings are **not verified account attestations** in PR 1. Later work
must calculate and verify them against authenticated OIDC material in protected
local credential storage. A syntactically valid user-supplied value confers
no access or proof of identity.

The v2 identity is deliberately separate from the historical
`provider.Config{...}` v1 identity: `provider.v2:sha256:...` binds a
canonical non-secret behavior digest plus the two opaque bindings. The
behavior digest hashes only typed non-secret configuration fields (endpoint,
model, wire, profile capabilities, RouteSafety and bounds, config version).
It does not hash raw OAuth credentials or arbitrary untyped options.

A frozen execution contract with a v2 wire identity is version 2; its canonical
JSON and hash incorporate the wire and both opaque bindings. V1 contracts
remain version 1 with their original canonical bytes. Unsupported schema
versions and mixed v1/v2 material fail closed.

## Activation gate

This PR **does not implement OAuth, secure credential custody, domain locks,
canonical state location, Responses SSE, quota management or live canaries**.
Both the compatibility composition and direct legacy Chat Completions adapter
refuse `responses_siwc_v1` instead of silently dispatching it as Chat
Completions. An operator cannot use SIWC from this version.

Remaining bounded deliveries:
1. Canonical SIWC CLI state domain across run/resume/inspect/decide (issue #177).
2. Interprocess lock, durable recovery and admission barrier.
3. Official PKCE/OIDC sign-in, protected refresh rotation and model catalog.
4. Direct Responses/SSE adapter and provider-governed multiple turns.
5. Full offline E2E tests and operational documentation.

The user-approved delivery plan and detailed acceptance criteria are in
[issue #174](https://github.com/Anakyklos/Runstead/issues/174). This offline
work never satisfies Gate A by itself; a live canary needs separate approval.

## Stage 2: canonical state-domain discovery

Stage 2 adds an opt-in `--state-domain siwc` selector for commands that access
durable state. Its locator is discovered at
`$XDG_STATE_HOME/runstead/siwc/siwc-locator.json`, falling back to
`$HOME/.local/state/runstead/siwc/siwc-locator.json`. The locator records one
absolute canonical state directory and a domain identifier; that directory
contains a versioned manifest and an already initialized `runstead.db`.
`--state-dir` and `RUNSTEAD_STATE_DIR`, when explicitly supplied with the SIWC
selector, must normalize to the registered path. Stage 2 reads these records
only: it does not create, repair or migrate a locator, manifest or database.
The locator schema is `{version, canonical_dir, domain_id}`; the manifest
schema is `{version, wire_contract, account_binding, credential_binding,
behavior_digest, provider_id, model, config_identity}`. Both use strict JSON
decoding with duplicate and unknown keys rejected. `domain_id` must equal the
manifest's v2 config identity, which binds the behavior digest and both opaque
bindings. The database must already be a valid initialized Runstead SQLite
store; the resolver checks it in immutable/read-only mode before handing its
path to the CLI. The database must contain the versioned `meta` key
`siwc_state_domain_v1` whose value exactly matches the manifest's
`config_identity`; this durable marker prevents a valid legacy SQLite file
from being claimed by a crafted locator and manifest. Stage 2 only verifies
this marker. Only a future authenticated registration flow may create it.
Existing WAL, SHM or journal sidecars cause a fail-closed refusal because
Stage 2 has no domain lock/reconciliation barrier.
Because Stage 3 has not added the interprocess lock and recovery barrier,
`run` and `resume` validate the SIWC locator and then refuse before opening
SQLite. `inspect`, `decide` and improvement commands may use the registered
database for their read or operator-controlled state operations.

Changing `HOME` or `XDG_STATE_HOME` can make the locator inaccessible. In that
case commands fail closed without creating a replacement database. The
operator must restore the original discovery environment. `XDG_DATA_HOME`
and legacy state-dir precedence continue to apply to non-SIWC providers.

The locator and manifest are not an authenticated account attestation. The
provider declaration's HMAC-shaped bindings are compared with the registered
manifest, never accepted as proof by themselves. Until a later authenticated
registration flow exists, there is no supported way to create a usable SIWC
domain; deterministic tests construct local synthetic fixtures. The Responses
adapter remains inactive and refuses dispatch. Stage 2 rejects symlink paths,
non-owner files, and directories or files accessible to other users. It does
not claim interprocess exclusion or complete TOCTOU protection; those belong
to Stage 3.
