# Sign in with ChatGPT — offline implementation sequence

Status: **Stage 4 implemented for explicit local authentication and custody; inference remains unavailable**. Maintainer decision: issue #174, authorized 2026-10-08.
No live login has been performed by this implementation. No inference or API key substitution is available.

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

These bindings are not accepted from provider configuration as proof. Stage 4
derives them from the locally protected binding key and verified OIDC issuer,
subject, issued client ID and stable host ID. Commands resolving a SIWC domain
verify the resulting bindings against that custody record both before and
after acquiring the Stage 3 domain lock.

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

Stages 1–3 establish the provider contract, canonical state discovery, Linux
interprocess exclusion, and durable recovery/admission boundary. Stage 4 adds
explicit official OAuth/OIDC registration, local credential custody, refresh,
revocation and an account-specific model catalog. It does **not** implement
Responses SSE, quota management or live canaries. Both the compatibility
composition and direct legacy Chat Completions adapter continue to refuse
`responses_siwc_v1` instead of silently dispatching it as Chat Completions.
Authenticated custody is not inference activation.

Remaining bounded deliveries:
1. Direct Responses/SSE adapter and provider-governed multiple turns (Stage 5).
2. Full offline E2E tests and operational documentation (Stage 6).

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
The read-only resolver accepts SQLite WAL/SHM/journal sidecars; command use
is permitted only after the Stage 3 lock is held and metadata is revalidated.
`state.Open` (which may create directories, migrate, checkpoint or recover
SQLite) is reached only while the SIWC domain lock is held.

Changing `HOME` or `XDG_STATE_HOME` can make the locator inaccessible. In that
case commands fail closed without creating a replacement database. The
operator must restore the original discovery environment. `XDG_DATA_HOME`
and legacy state-dir precedence continue to apply to non-SIWC providers.

The locator and manifest are not an authenticated account attestation. The
provider declaration's HMAC-shaped bindings are compared with the registered
manifest and verified against protected custody, never accepted as proof by
themselves. Stage 4's explicit `runstead siwc setup` is the only supported way
to create a new SIWC domain; deterministic tests also construct local synthetic
fixtures. The Responses adapter remains inactive and refuses dispatch. Stage 2 rejects symlink paths,
non-owner files, and directories or files accessible to other users. Stage 3 supplies cooperative interprocess exclusion for the CLI paths described below. It does not claim protection from a same-user process that bypasses the CLI and directly edits the database.

## Stage 3: Linux lock and recovery/admission barrier

All SIWC CLI commands that open the store (`run`, `resume`, `inspect`,
`decide`, and every `improvement` subcommand) take the same persistent
`.siwc-domain-lock-v1` file inside the registered canonical directory. Linux
`flock(LOCK_EX|LOCK_NB)` is retried for at most 250 ms at 10 ms intervals;
contention returns a bounded refusal. The lock file is versioned and bound to
the manifest's `config_identity`, has private owner-only permissions, is
opened without following symlinks, and is never unlinked on release or
recovery. Crash termination releases the kernel lock while preserving the
inode/marker for the next process. Only ext2/3/4, XFS, Btrfs, tmpfs and
overlayfs are accepted; other or unverifiable filesystems fail closed.

The resolver's preflight uses immutable SQLite reads and does not create or
repair files. It rejects database, locator, manifest and present SQLite
sidecars unless they are private regular files with `st_nlink == 1`; it also
confirms the database and sidecar inode identities remain stable through
the immutable check. Each command then acquires the lock and repeats locator,
manifest, file-link and DB-marker validation before `state.Open`. For `run`,
persisted
`prepared`, `running`, `uncertain` or `human_review_required` provider attempts
block a fresh task admission. `resume` restores the singleton governor state
and runs the existing reconciliation pipeline under the lock before a new
provider admission. Prepared/uncertain attempts retain their ledger/history
and conservative debit; they are not replayed. Operator approval is a durable
pause that returns control and releases the lock; `decide` later acquires the
same domain lock. `inspect` holds the lock through its complete rendered
snapshot, and improvement artifact writes remain inside the critical section.

The tests prove Linux process contention and automatic lock release after
process termination on the test filesystem, stable marker reuse, and
two-directory hardlink-alias rejection by competing processes before either
can enter the state critical section. A CLI E2E kills a process after its
prepared provider attempt and governor debit are durable, then runs the real
`runstead resume --state-domain siwc` twice in fresh processes and checks the
reconciled attempt, ledger, governor singleton and refusal to infer. The
allowlist does not prove behavior on every local filesystem or protect
against a same-user process that directly edits SQLite without using Runstead.
The immutable preflight can only inspect the
main DB image; durable SQLite sidecar replay is performed by SQLite when the
locked store opens. Synthetic offline fixtures do not constitute account
authentication or Responses inference evidence.

## Stage 4: official OAuth/OIDC custody and model catalog

`runstead siwc login` explicitly opens the system browser and binds one
Authorization Code + PKCE S256 flow to a loopback listener on
`127.0.0.1:<ephemeral-port>/auth/callback`. The first registration sends the
vendor's dynamic `client_id=dynamic_agent_client` and `agent_name_hint=Runstead`;
the issued client ID from the callback is used for token exchange and later
requests. A stable owner-only `urn:uuid:...` host ID is stored locally and
reused. State, nonce and PKCE verifier are freshly generated per login and are
not printed. The production endpoints are fixed to official issuer/API hosts;
HTTP endpoints are accepted only by internal offline test seams on loopback.
Redirects are not followed.

The token response must grant every documented scope. The ID token is checked
against issuer discovery/JWKS for RS256 signature, issuer, client audience,
subject, nonce, expiry and client ID. The access token is separately checked
for signature, issuer, API resource audience, subject, client ID, expiry and
exact granted scope set before it is used to query the catalog. The catalog
retains only exact model slugs with `visibility=list`; that metadata does not
establish entitlement or provider eligibility. `runstead siwc setup` requires
an explicit account and model and records exactly one provider/model and
credential/account binding in the domain. It does not enable inference.

On Linux, custody lives separately from SQLite under
`$XDG_CONFIG_HOME/runstead/siwc`, or `$HOME/.config/runstead/siwc` when XDG is
unset. Directories are owner-only mode 0700; files are owner-only mode 0600,
regular, single-link files. Writes use a synced temporary file, atomic rename
and directory sync. The stable host ID and local 256-bit binding key are
protected alongside per-registration credential records. Tokens never enter
SQLite, provider contracts, traces, evidence or CLI output. This is filesystem
permission protection, not hardware-backed encryption: a same-user process or
offline theft of the user's disk is outside this protection boundary.

Refresh takes a separate persistent flock-protected custody lock and writes a
durable uncertainty marker before making one refresh request. Rotated tokens
and expiry are atomically committed before that marker is cleared. If the
response is lost, the process crashes during rotation, or marker cleanup is
ambiguous, that registration is not refreshed or replayed again; the operator
must sign in again. Revocation is explicit. If the issuer cannot confirm it,
local tokens are retained and the command reports failure. A successful
revocation clears tokens but retains non-secret registration identity for
inspection. A new successful explicit login for the same subject/client
replaces the uncertain rotation and clears its marker durably. Multiple saved
registrations are permitted but never pooled or automatically switched.

The implementation and fixtures are fully offline-testable; no real sign-in,
token exchange, catalog request or inference was executed. The tested Linux
filesystem allowlist does not establish semantics for unlisted or remote
filesystems. Stages 5 and 6 remain pending, `responses_siwc_v1` remains refused
by the Chat Completions adapter, and this work does not satisfy Gate A or
promote M12.
