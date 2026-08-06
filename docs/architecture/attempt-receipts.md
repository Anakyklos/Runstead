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

The receipt set is deliberately request-scoped and in-memory. Persistence,
automatic activation, CLI configuration, streaming terminal artifacts, and
live rollout policy are outside this boundary.
