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

The offline deliveries through Stage 3 establish canonical state discovery,
Linux interprocess exclusion, and a durable recovery/admission boundary. They
do **not** implement OAuth, protected credential custody, account registration,
Responses SSE, quota management or live canaries. Both the compatibility
composition and direct legacy Chat Completions adapter continue to refuse
`responses_siwc_v1` instead of silently dispatching it as Chat Completions.
SIWC authentication and inference remain inactive.

Remaining bounded deliveries:
1. Official PKCE/OIDC sign-in, protected refresh rotation and model catalog.
2. Direct Responses/SSE adapter and provider-governed multiple turns.
3. Full offline E2E tests and operational documentation.

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
manifest, never accepted as proof by themselves. Until a later authenticated
registration flow exists, there is no supported way to create a usable SIWC
domain; deterministic tests construct local synthetic fixtures. The Responses
adapter remains inactive and refuses dispatch. Stage 2 rejects symlink paths,
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
