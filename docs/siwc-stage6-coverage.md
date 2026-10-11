# SIWC Stage 6 offline acceptance matrix

Parent issue: [#174](https://github.com/Anakyklos/Runstead/issues/174)  
Stage 6 issue: [#185](https://github.com/Anakyklos/Runstead/issues/185)  
Reviewed baseline: `c8e5b4946a576ccd022a648c2d0caa0161f23c66`

**OFFLINE ONLY. Gate A NOT SATISFIED.** All positive and negative fixtures use
synthetic credentials, local state, loopback endpoints, or subprocesses. No
live login, refresh, catalog query, model inference, quota check, or canary is
part of this acceptance.

The status names below are literal matrix values. `EXISTENTE` means the named
baseline test already proves the property; `ADICIONADO` identifies the Stage 6
delta; `LACUNA` identifies evidence still required; `BLOQUEADO` records a claim
that cannot be made within the approved offline scope. Test level is stated so
unit coverage is not mistaken for process-level evidence.

| Requirement | Status | Test evidence and level |
| --- | --- | --- |
| Legacy v1 compatibility, typed v2 provider identity, and frozen execution contract bytes/hash | EXISTENTE | Unit: `internal/config.TestLegacyProvidersDocumentRejectsPresenceOfSIWCFieldsEvenWhenEmpty`, `TestSIWCV2DocumentParsesTypedIdentityWithoutActivation`, `TestSIWCV2ChangesBindingWithoutManualConfigVersionBump`; `internal/composition.TestLegacyV1FrozenContractMatchesPreSIWCBaseline`, `TestSIWCFrozenContractV2AndLegacyV1RemainSeparate`; `internal/state.TestLegacyTaskWithoutExecutionContractRemainsCompatible`, `TestExecutionContractHashBindsExactPersistedBytes`. |
| Account/model selection, custody binding checks, and pre-dispatch refusals | EXISTENTE | Unit/integration: `internal/siwcauth.TestWithAccessTokenVerifiesModelBindingsAndSerializesLogout`, `TestWithAccessTokenRefusesExpiredChangedAndForgedBindings`, `TestBindingsAreOpaqueAndRegistrationRedactsTokens`; CLI: `cmd/runstead.TestSIWCSetupCommitsWithCurrentAuthenticatedRegistration`, `TestSIWCSetupPublishesProviderAndDomainTransactionally`; adapter: `TestConstructorRequiresCapabilitiesAndOpaqueBindings`. |
| Canonical domain, override agreement, permissions, aliases, process lock, and conservative recovery | EXISTENTE | Unit: `internal/siwcstate.TestResolveRegisteredDomainAndEquivalentOverride`, `TestResolveRejectsIdentityMismatch`, `TestResolveRejectsDatabaseHardlinkAlias`, `TestResolveRejectsSymlinksAndWeakPermissions`, `internal/siwcstate.TestAcquireLockExcludesProcessesAndCrashReleases`; process E2E: `cmd/runstead.TestSIWCDatabaseHardlinkAliasesRefuseConcurrentDomainEntry`, `TestSIWCPreparedAttemptRecoveryAcrossProcessCrash`, `TestSIWCDispatchedProcessCrashResumesWithoutReplay`. |
| Real CLI process through loopback Responses/SSE, multiple governed turns, durable observations/effects, and independent verifier | ADICIONADO | Process E2E: `cmd/runstead.TestSIWCResponsesMultiTurnCLIProcessPersistsTwoGovernedCalls` asserts exactly two physical POSTs, two completed provider attempts, two governor ledger rows, the completed action/tool attempt, successful observation, and persisted passing verifier/check rows in SQLite. |
| Malformed, failed, incomplete, missing-terminal, truncated, canceled, HTTP/SSE failures, sanitized classification, and no partial success | EXISTENTE | Adapter unit/HTTP integration: `internal/provider/responsessiwc.TestSSERequiresOneValidTerminalAndRejectsPartialText`, `TestPreflightAndHTTPFailuresNeverAmplify`, `TestStalledStreamHonorsCallerTimeoutWithoutRetry`, `TestHTTPFailurePreservesSanitizedRequestIDAndStatus`, `TestCancelBeforeDispatchAndRedirectNotFollowed`, `TestCancelAfterDispatchKeepsUncertainDeliveryAndOnePost`; classifier unit: `internal/provider/compat.TestSIWCFailureClassificationIsSanitizedAndNeverRetryable`. |
| Crash before/after dispatch, durable uncertain debit, reconciliation, and no replay of completed effects | EXISTENTE | Process E2E: `cmd/runstead.TestSIWCPreparedAttemptRecoveryAcrossProcessCrash`, `TestSIWCDispatchedProcessCrashResumesWithoutReplay`; recovery unit/integration: `internal/state.TestRecoveryReasonsAreRedacted`, `internal/governor.TestUncertainOutcomeRemainsDebited`, `TestCancellationAfterStartIsUncertainAndStillDebited`. |
| Retry/fallback/account/model/provider switching and hidden request amplification are refused or absent | EXISTENTE | Adapter unit/HTTP integration: `internal/provider/responsessiwc.TestPreflightAndHTTPFailuresNeverAmplify`, `TestCancelAfterDispatchKeepsUncertainDeliveryAndOnePost`; governed integration: `internal/provider/responsessiwc.TestTwoGovernedCompletesSendSelfContainedTranscriptOnceEach`; CLI process tests: `cmd/runstead.TestSIWCRetryPolicyRefusesBeforeSIWCDomainMutation`, `TestSIWCResumeRetryPolicyRefusesBeforeDomainMutation`, `TestResumeInvalidProviderConfigurationFailsBeforeOpeningAlternateState`; shared integration matrix: `internal/provider/compat.TestMatrixRedirectRefusedWithoutSecondRequest`, `TestMatrixCancellationAfterPossibleDispatchIsConservativeAndNeverReplays`. |
| Secret redaction across persistence, context, recovery, identity, traces, setup/status, and process output | EXISTENTE | Unit/integration: `internal/state.TestRedactJSON`, `TestProviderPromptNeverPersisted`, `TestRecoveryReasonsAreRedacted`, `internal/context.TestRedactionNoSecretsInProjection`, `internal/trace.TestPolicySinkAttemptTelemetryRedaction`, `internal/provider/compat.TestMatrixSecretMinimization`; CLI/process E2E: `cmd/runstead.TestSIWCStatusDoesNotRenderCredentialMaterial`, `TestSIWCResponsesMultiTurnCLIProcessPersistsTwoGovernedCalls`, `TestProviderE2ECodingLoopAcrossAllFamilies`. Raw SQLite bytes and rendered surfaces are checked by the respective fixtures. |
| Live account eligibility, production filesystem behavior outside the tested Linux allowlist, or Gate A | BLOQUEADO | Not established by offline fixtures and intentionally outside Stage 6 authorization. No live SIWC operation or Gate A PASS may be inferred. |

## Authority and boundaries

The tests leave `provider_id`, `protocol_family`, and `wire_contract` as
separate identities. The governor owns admission and debit; SQLite owns durable
task and effect history; recovery reconciles uncertain work without replay;
the verifier owns acceptance. Responses session IDs and model statements do
not become task truth. No attempt can hide a second HTTP request, and SIWC does
not gain automatic retry, fallback, or account/model/provider rotation.

This matrix records offline software evidence only. It does not prove OpenAI
account access, model entitlement, live transport compatibility, quota, or
operational readiness.
