# Authoritative upstream attempt receipts

Runstead treats an OmniRoute response as a logical completion. A protected
account lane must debit the number of upstream attempts that OmniRoute actually
started, including retries, credential refreshes, fallbacks, combos, cooldown
replays, timeouts, and cancellations after the upstream call began.

## Contract

The versioned receipt set is carried in the `X-OmniRoute-Attempt-Receipts`
response header when a non-streaming response has reached a finalized state.
Runstead sends `X-Runstead-Client-Request-Id` and requests the extension with
`X-Runstead-Attempt-Receipts: v1`.

Each receipt contains a schema version, immutable attempt ID, correlated client
request ID, contiguous sequence, provider, model, stable account-lane hash,
start and completion timestamps, typed outcome and trigger, and
`upstream_reached`. Receipt payloads never contain prompts, responses, cookies,
tokens, keys, raw headers, or direct account identifiers.

The consumer rejects unknown versions and fields, missing or duplicate IDs,
correlation or route mismatches, sequence gaps, invalid outcomes or triggers,
bad timestamps, oversized identifiers, more than 16 receipts, or serialized
payloads over 8 KiB. Missing or invalid authority fails closed and blocks later
admission; Runstead does not invent a charge for an unverified attempt.

## Accounting

`StartReceiptAware` reserves the logical request without debiting the initial
attempt. `FinishWithAttemptReceipts` validates the complete set under the
governor lock and debits exactly one ledger/task/telemetry unit per receipt.
The initial receipt is therefore charged once, while a two-receipt retry chain
is charged twice. An uncertain, timeout, or cancelled receipt is still
consumed and makes the logical outcome uncertain. A sanitized
`upstream_attempt` event is emitted for every receipt.

This contract is preparatory until a caller explicitly supplies a receipt-aware
provider client. It does not activate a protected live path or close any
separate activation or operational configuration work.
