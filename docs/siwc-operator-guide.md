# SIWC operator guide

This guide documents the implemented Linux SIWC path. SIWC is selected
explicitly through the `responses_siwc_v1` wire contract and
`--state-domain siwc`; it does not change Runstead's provider-neutral runtime.
The provider ID, protocol family and wire contract remain separate identities.
The governor, policy, approvals, SQLite records, recovery, evidence and
verifier remain authoritative.

## Support and boundaries

SIWC custody commands and the canonical SIWC state-domain path are supported
only on Linux. The state-domain lock accepts ext2/3/4, XFS, Btrfs, tmpfs and
overlayfs; unrecognized, remote or otherwise unverifiable filesystems fail
closed. The lock coordinates Runstead CLI processes. It does not prevent a
same-user process from directly editing SQLite outside Runstead.

Credential custody uses owner-only files under
`$XDG_CONFIG_HOME/runstead/siwc`, or `$HOME/.config/runstead/siwc` when
`XDG_CONFIG_HOME` is unset. SQLite and the registered SIWC state domain are
separate, under the SIWC locator rooted at `$XDG_STATE_HOME/runstead/siwc`
(falling back to `$HOME/.local/state/runstead/siwc`). Custody holds renewable
tokens and local binding material; SQLite holds task truth, provider attempts,
governor accounting, evidence and the non-secret domain marker and bindings.
Tokens and refresh credentials are not stored in SQLite. File permissions do
not protect against the same user or offline disk theft.

An offline test or local fixture does not authenticate an account, prove model
entitlement, establish live compatibility or qualify a canary. This guide's
management commands can contact official services when an operator invokes
them. The Stage 6 acceptance work is **OFFLINE ONLY** and does not run those
commands against real services. **Gate A NOT SATISFIED** by offline validation.

## Credential and model commands

`runstead siwc --help` lists the implemented subcommands:

```text
runstead siwc login
runstead siwc status
runstead siwc models --client-id CLIENT_ID
runstead siwc setup --client-id CLIENT_ID --model EXACT_MODEL \
  --state-dir /absolute/new/state-dir --providers /absolute/new/providers.json
runstead siwc logout --client-id CLIENT_ID
```

- `login` opens the system browser and performs the official OAuth/OIDC flow
  using a loopback PKCE callback. `login --client-id CLIENT_ID` is the
  implemented reauthorization form. This can contact the identity service.
- `status` lists saved local registration metadata, a short subject binding,
  selected model, scopes and local credential state. It does not print tokens.
- `models --client-id CLIENT_ID` requests that account's visible model catalog.
  Catalog visibility is display metadata, not entitlement or eligibility.
- `setup` takes an exact model slug, checks it against the selected account's
  catalog, and transactionally creates one provider declaration and a new
  canonical state domain. Both output paths must be normalized absolute paths;
  the state path must be new. This operation can refresh credentials or
  contact the catalog service. The generated provider ID defaults to
  `siwc-openai`; `--provider-id ID` is an implemented optional override.
- `logout --client-id CLIENT_ID` requests revocation. Local tokens are cleared
  only after revocation is confirmed; on ambiguity or failure, credentials
  are retained and the operator must inspect the result before retrying.

`models`, `setup`, login/reauthorization and `logout` are explicit management
operations, not part of offline validation. Do not run them as a test against
real credentials unless a separate live operation is authorized. Never put a
credential, access token, refresh token, private URL or test endpoint in a
provider file, task prompt, fixture, log or report.

## Explicit governed execution

The `setup` output provider file contains one exact model and opaque account
and credential bindings. Keep that file private. For a deliberately authorized
operation, the implemented command shape is:

```text
runstead run --task "TASK" --workspace /absolute/workspace \
  --providers /absolute/providers.json --provider-id siwc-openai \
  --state-domain siwc --acceptance /absolute/acceptance.json
```

`--state-domain siwc` is required for this provider. It binds execution to the
registered canonical domain; explicitly supplied `--state-dir` or
`RUNSTEAD_STATE_DIR` must resolve to the registered directory. The selected
provider declaration, model and bindings are frozen into task/attempt identity.
Changing provider, model, discovery root or bindings fails closed. There is no
automatic account, model or provider selection.

Completion also requires an operator-owned acceptance plan. Save a typed JSON
plan such as the following as `/absolute/acceptance.json` before starting the
task:

```json
{
  "version": 1,
  "checks": [
    {"id": "readme-present", "type": "file_exists", "path": "README.md"}
  ]
}
```

Every check in the plan is mandatory, paths are relative to the workspace, and
the plan is frozen with the task. The example run command supplies this plan
through `--acceptance FILE`.

The accepted check types are `file_exists`, `file_absent`, `file_hash` and
`recipe_exit_zero`; each requires its documented typed fields. A
`recipe_exit_zero` check only passes after evidence from an executed declared
recipe with the matching ID. Declare that recipe in the operator-controlled
catalog and supply it with `--recipes FILE`; without a matching declared
recipe and successful execution, the verifier blocks completion. A recipe
check does not itself authorize execution: recipe policy and any required
approval still apply. Do not weaken or omit checks to make a task complete.

Each completion is governor-admitted and makes one Responses POST. SIWC does
not automatically retry, fall back or rotate accounts/models/providers. A
request with uncertain delivery remains accounted for and is not replayed by
recovery. Do not add a retry policy to SIWC; retry configuration is refused
before dispatch.

## Inspect, resume and approvals

Use the same configured provider and state domain when resuming a configured
SIWC task:

```text
runstead inspect TASK_ID --state-domain siwc
runstead resume TASK_ID --providers /absolute/providers.json \
  --provider-id siwc-openai --state-domain siwc
```

If approval is required for a write or recipe effect, the task pauses with a
durable pending action. Review it with `inspect`, then explicitly approve or
reject through the implemented operator command:

```text
runstead decide TASK_ID ACTION_ID approved --state-domain siwc
# or
runstead decide TASK_ID ACTION_ID rejected --state-domain siwc
```

Approval is not inferred from model output. After `decide`, resume the task
with the same provider identity and domain. The verifier independently checks
persisted evidence and observable state; model text claiming completion is
not proof.

## Lock, conflict and recovery

Commands that open the SIWC database acquire the same persistent, exclusive
Linux `flock` domain lock, then revalidate the locator, manifest, bindings,
file identity and database marker before opening mutable SQLite state. Lock
contention has a bounded wait and fails closed. The lock file is retained after
release so cooperating processes address the same inode.

If a process exits during a request, the kernel releases its lock. Keep the
domain files in place and run `inspect` before deciding what to do. `resume`
reconciles durable attempts under the lock. A prepared or uncertain dispatch
is not replayed; governor debit and attempt history are retained, and an
unreconciled effect may require human review. Do not delete the locator,
manifest, database, WAL/SHM files, lock marker, custody records or uncertainty
markers to clear an error. Do not move the state directory while a Runstead
process is using it. Preserve the full directory and custody store before
manual recovery or support investigation.

During failed initial setup, Runstead removes only artifacts whose recorded
identity still matches. If unexpected files or identity changes exist, setup
leaves the locator unpublished and preserves remaining state. Inspect and
preserve that content before relocating or removing anything; do not use
recursive cleanup as recovery.

## Errors requiring operator attention

The Responses adapter returns sanitized typed classifications including
`config_refused`, `auth_unavailable`, `authentication_denied`,
`permission_denied`, `rate_or_capacity`, `upstream_server_failure`,
`cancelled`, `timeout`, `transport`, `malformed_response`,
`invalid_envelope`, `response_too_large`, `request_too_large`,
`unsafe_redirect`, `stream_incomplete`, `response_failed` and
`response_incomplete`. A typed classification is evidence about one attempt;
it does not authorize another attempt.

Stop and inspect before acting when identity/binding validation fails, a
domain lock is busy, filesystem or permission checks fail, setup reports
preserved incomplete state, refresh is marked uncertain, revocation was not
confirmed, delivery is uncertain, recovery requires human review, or provider
and persisted task identity differ. For refresh uncertainty, the implemented
custody behavior requires a fresh explicit sign-in; it does not replay the
ambiguous refresh. For uncertain inference or effects, preserve durable
accounting and use the normal reconciliation/human-review path rather than
repeating the task blindly.

## Offline validation, live eligibility and Gate A

Offline acceptance uses synthetic credentials, local fixtures, loopback
HTTP/SSE and local processes. It may establish parser/contract behavior,
one-request attempt accounting, durable SQLite state, redaction and specified
crash-recovery behavior for those fixtures. Label each result by evidence
level: unit, integration or process E2E. A passing test is not a live service
observation.

Live eligibility is a separate question requiring separately authorized
account, model, route and operational evidence. The `models` catalog is not
eligibility proof. Gate A is a distinct live-canary gate; it cannot be inferred
from offline tests, setup success, visible model metadata or this guide.
Current Stage 6 status and limitations are recorded in the
[offline implementation plan](siwc-offline-plan.md).
