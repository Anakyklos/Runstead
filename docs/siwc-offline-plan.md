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
1. Canonical SIWC CLI state domain across run/resume/inspect/decide.
2. Interprocess lock, durable recovery and admission barrier.
3. Official PKCE/OIDC sign-in, protected refresh rotation and model catalog.
4. Direct Responses/SSE adapter and provider-governed multiple turns.
5. Full offline E2E tests and operational documentation.

The user-approved delivery plan and detailed acceptance criteria are in
[issue #174](https://github.com/Anakyklos/Runstead/issues/174). This offline
work never satisfies Gate A by itself; a live canary needs separate approval.
