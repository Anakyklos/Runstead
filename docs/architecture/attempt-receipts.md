# Attempt-receipt boundary

The provider-neutral receipt types live in
`internal/provider/attempt_receipts.go`. The OmniRoute adapter consumes the
response header in `internal/provider/omniroute/client_transport.go` and
validates it against the request in `internal/provider/omniroute/client.go`.

The governor uses `provider.AttemptReceiptAware` as a capability boundary.
Receipt-aware execution passes the client request ID to the provider, starts
with `Permit.StartReceiptAware`, and finishes with
`Permit.FinishWithAttemptReceipts`. Legacy single-attempt clients retain the
existing `Start`/`Finish` path and must explicitly declare disabled
amplification in `provider.RouteSafety`.

M1 accepts exactly one receipt per completion, using one provider, one concrete
model and one account lane. `ModelPool` remains an allowance bucket; it is not
a model identity.
Pooling, automatic fallback, combo routing, internal retries and cooldown
replays are rejected by the M1 receipt route declaration. An executor retry is
a new governed completion, not an additional receipt in the previous one.

Receipt outcomes map to the governor's typed circuit classes. Reconciliation
processes every receipt before the logical response outcome, so an internal
security or rate signal cannot be erased by a later success. Receipt
timestamps must fit the real permit interval and receipt attempt IDs are
unique within the bounded three-hour protection horizon of the governor.

If the finalized receipt set is missing or invalid after a provider call,
Runstead records one conservative uncertain debit, emits an
`uncertain_attempt` event, marks telemetry unsafe and blocks later admission.
This is deliberately conservative recovery for connection loss; it is not a
claim that the exact upstream attempt count was recovered.

The receipt set is request-scoped and in-memory. Persistence, automatic
activation, CLI configuration, streaming terminal artifacts, and live rollout
policy are outside this boundary.
