# Chaos and interruption hardening (issue #13)

This document is the auditable failure matrix of the deterministic chaos and
interruption suite. It maps every failure case of issue #13 to the test that
proves the protected property. The suite runs entirely offline: fake
providers, fake clocks, deterministic crash seams, controlled subprocesses and
temporary isolated workspaces. No live network, no live OmniRoute, no
credentials, no timing races and no probability.

The core invariant the whole matrix protects:

> Runstead can be interrupted, deceived, limited, crashed or faced with
> uncertain effects without turning uncertainty into success, without losing
> its local truth and without silently repeating completed effects.

## How failures are injected

| Seam | Location | Purpose |
| --- | --- | --- |
| `state.SetCrashPoint` | `internal/state` | deterministic process death at named persistence boundaries (TX 1, TX 2, finalize, verification, recovery) |
| `tools.SetWriteCrashPoint` | `internal/tools` | death before/after the write effect (before result persistence) |
| `recipe.SetCrashPoint` | `internal/recipe` | death right after the process started (effect provably in flight) |
| `faultyPersistence` | test-only wrapper over `state.Persistence` | SQLite failure injection per named method (fails the Nth call) |
| `chaosProvider` / `receiptChaosProvider` | test-only fake clients | scripted provider errors classified by the real `omniroute.Classify`, scripted attempt-receipt sets |
| subprocess crash helpers | `cmd/runstead` | the real CLI composition dies at a boundary and the parent resumes it |

Production code contains only the three tiny crash-seam hooks (nil in
production); everything else is test code.

## Failure matrix

### 1. Model / protocol chaos

| Failure case | Test | Property protected |
| --- | --- | --- |
| empty model response | `TestProtocolChaosMatrix/empty_model_response` | typed `provider_failure` + `empty_response` classification, one governed attempt, task finalized failed, no actions |
| truncated response | `TestProtocolChaosMatrix/truncated_response_envelope` | bounded corrections (`corrections_exhausted`), no effect executed |
| malformed action JSON | `TestProtocolChaosMatrix/malformed_action_JSON` | bounded corrections, no effect executed |
| multiple conflicting actions | `TestProtocolChaosMatrix/multiple_conflicting_action_envelopes` | rejected as one parse failure, only the corrected single action executes |
| unknown tool | `TestProtocolChaosMatrix/unknown_tool` | corrected, never executed, no tool attempt |
| identical repeated action | `TestProtocolChaosMatrix/identical_repeated_action` + `TestProtocolChaosRepeatedActionRejectedProjection` | repeat guard stops typed; guard-rejected proposals persist as `rejected`, never `planned`/`completed` |
| completion without evidence | `TestProtocolChaosMatrix/completion_without_evidence` | `final_not_grounded`, never `completed` |
| model claims completion while suite red | existing `TestLoopVerificationClaimedFileMissingNeverCompletes`, `TestCodingLoopPrematureCompletionFailsThenPasses` (reused) | verifier rejects the claim and returns the task to execution |

Parser-level cases (truncated, malformed, multiple envelopes, unknown tool)
are additionally unit-tested in `internal/protocol/parser_test.go` and the
shared corpus in `internal/agent/corpus_test.go`; the matrix adds the runtime
typed-outcome and durable-state evidence.

### 2. Provider failures

| Failure case | Test | Property protected |
| --- | --- | --- |
| timeout | `TestProviderChaosTimeoutClassifiedBounded` | classified `timeout`, exactly one attempt, one ledger debit, no hidden retry, durable attempt row carries outcome |
| connection reset | `TestProviderChaosConnectionResetClassified` | classified `connection_reset`, single attempt |
| retry never bypasses the governor | `TestProviderChaosNoRetryBypassesGovernor` | no re-issue outside the governor; lane fully released |
| duplicated provider response | `TestProviderChaosDuplicatedResponseNoDuplicateEffects` | identical re-proposal executes as a new attempt and fails closed on the stale-state precondition; one completed write, no silent duplication |
| stale/missing remote session, replacement with a new conversation | `TestProviderChaosStaleSessionMetadataDisposable` | old session id is disposable metadata; the resumed run continues from authoritative local state (evidence, attempts), new conversation receives the reconstructed context, ledger continues |
| cancellation mid-flight | existing `TestLoopCanceledDuringProviderIO` (reused) | conservative accounting: exactly one debit, never zero, never two |
| session replaced after interruption | existing `TestLoopResumeContextReachesNewConversation` (reused) | new conversation receives reconstructed context, old assistant turns never embedded |

The live OmniRoute path stays conditioned on #29 -> #30 -> #4; this suite
proves the contract with fixtures and fakes.

### 3. Process execution chaos (issue #26 recipes only)

| Failure case | Test | Property protected |
| --- | --- | --- |
| recipe exceeds timeout | `TestProcessChaosTimeoutBoundedAndNeverCompletes` | stuck command terminated within the configured timeout, full tree dead, evidence records timeout + negative exit code, narration never completes |
| oversized stdout / stderr | `TestProcessChaosOversizedOutputTruncationExplicit` | limits independent (64/32 bytes), truncation explicit, real byte counts preserved, truncated evidence can never pass a `require_untruncated` check even with exit 0 |
| partial output before timeout | `TestProcessChaosPartialOutputBeforeTimeout` | partial output preserved as evidence, timeout recorded, narration cannot complete |
| killed process | `TestProcessChaosKilledProcessNeverBecomesSuccess` | real terminating signal + negative exit code preserved; a dead process is never success |
| crash mid-process (effect in flight) | `TestCodingLoopCrashMidProcessRequiresHumanReview` | prepared process attempt escalates to `human_review_required` on resume; never re-run, never fabricated into success |
| process finished but result not persisted | `TestRuntimeCrashBeforeToolTX2LeavesPreparedEffectReturned` (state-level) + `tool_tx2_before` crash coverage | attempt stays prepared, no citable evidence, recovery never blindly re-runs |
| exit code preservation | existing `TestProcessRunnerExitNonZero`, `TestProcessRunnerTerminatingSignal`, `TestProcessRunnerTimeoutKillsFullTree` (reused) | real exit status and terminating signal preserved by the runner |

### 4. Crash / interruption boundaries

| Boundary | Test | Property protected |
| --- | --- | --- |
| before the effect (intent persisted, effect not started) | existing `TestRuntimeCrashBeforeWriteEffectKeepsFileUnchanged` (reused) | recovery does not presume the effect happened; file unchanged; reconciliation `write_effect_not_started` |
| after first write effect (coding loop) | `TestCodingLoopCrashAfterFirstWriteReconcilesAndCompletes` | write reconciled from filesystem as completed, never re-executed; task completes with a new conversation |
| after failing test evidence | `TestCodingLoopCrashAfterFailingTestResumesWithoutRerun` | failing test never re-run after resume; diagnosis continues; exactly 3 recipe runs total |
| after corrective write | `TestCodingLoopCrashAfterCorrectiveWriteReconcilesAndReruns` | corrective write reconciled as completed; passing rerun runs once; completes |
| after passing verifier, before finalize | `TestCodingLoopCrashAfterPassingVerifierResumesWithoutReExecution` | restart does not repeat effects (3 reads / 2 writes / 3 recipes unchanged); completion re-decided by the verifier from persisted history |
| mid provider effect | existing `TestCrashMidProviderEffectParent` (reused) | provider attempt stays prepared/uncertain, never success |
| mid write transaction | existing `TestCrashMidWriteTransactionParent` (reused) | SQLite transaction rolls back atomically |
| unreconcilable write | existing `TestRuntimeUnreconcilableWriteStopsWithHumanReview` (reused) | third-party state change escalates to `human_review_required` |

### 5. Database failure injection

| Failure case | Test | Property protected |
| --- | --- | --- |
| failure before any state persists | `TestPersistenceChaosCreateTaskFailure` | run stops `persistence_failure`, no task row, no attempt |
| failure persisting action intent | `TestPersistenceChaosActionRecordFailure` | effect never executes, workspace untouched |
| failure at TX 1 (durable intent) | `TestPersistenceChaosTX1FailureProvesEffectNeverStarts` | effect never starts |
| failure at TX 2 (result commit) | `TestPersistenceChaosTX2FailureKeepsEffectUnrecorded` | effect happened but state does not advance; attempt stays prepared, no evidence, outcome typed `persistence_failure`, never completed |
| failure at finalize | `TestPersistenceChaosFinalizeFailureLeavesResumableState` | history intact, task stays running (resumable), never completed |
| failure persisting verification | `TestPersistenceChaosVerificationPersistFailureBlocksCompletion` | completion gate cannot be proven durable -> `verification_blocked`, task stays resumable |

No SQLite error is ever converted into task success; the store is never
turned into a fault-injection framework (one test-only wrapper, ~40 lines).

### 6. Governor / #58 chaos matrix (runtime level)

The PR #61 unit tests remain the primary evidence for allowance semantics;
these runtime tests prove the same invariants through the real loop + real
store where that adds new evidence.

| Failure case | Test | Property protected |
| --- | --- | --- |
| CAPTCHA under unlimited text | `TestUnlimitedTextRuntimeSevereSignalOpensHumanReviewCircuit` | classified failure opens the human-review circuit; circuit survives restart; the next run is refused `account_circuit_open`; never completed |
| one in-flight attempt under unlimited text | `TestUnlimitedTextRuntimeConcurrentLoopsStaySerialized` | two concurrent loops: max concurrency exactly 1, both complete |
| unknown profile local ceilings | `TestUnknownRuntimeCeilingsStopLoopAndNeverPromote` | conservative local ceiling stops a real loop typed; success never promotes `unknown` |
| hidden amplification under unlimited text | `TestUnlimitedTextRuntimeHiddenAmplificationFailsClosed` | every authoritative receipt debited (2), telemetry unsafe, lane blocked, one provider call, never completed |
| receipt positive control | `TestUnlimitedTextRuntimeSingleReceiptCompletes` | one receipt per logical attempt completes and debits exactly one |
| profile transition across runtime restart | `TestProfileTransitionAcrossRuntimeRestartPreservesDurableProtection` | published_quota -> unlimited_text preserves ledger, task attempts and cooldown; no fresh logical account; persisted projection keeps the new kind without reset |
| transition/restart durability | existing `TestAllowanceKindTransitionDoesNotResetDurableState`, `TestGovernorAllowanceKindTransitionSurvivesPersistence`, `TestUnlimitedPersistedStateSurvivesRestart` (reused) | ledger/attempts/circuit/cooldown survive every profile transition |

`unlimited_text` never means ungoverned: pacing, serialization, task budgets,
loop guards, Retry-After/cooldown, rate/capacity signals and severe-signal
circuit behavior stay active and are proven at runtime.

### 7. Existing coverage explicitly reused (not duplicated)

- `TestLoopRepeatedActionGuardStopsWithoutReExecution`, `TestLoopZeroRepeatedActionsStopsImmediately` (repeat guard);
- `TestLoopFabricatedEvidenceRejected`, `TestLoopVerificationClaimedFileMissingNeverCompletes` (unsupported claims);
- `TestLoopProviderFailureClassified`, `TestLoopAccountCircuitOpen` (classified provider failure, circuit);
- `TestLoopCanceledDuringProviderIO`, `TestLoopCanceledWhileQueuedConsumesNoAttempt` (cancellation accounting);
- `TestCrashMidProviderEffectParent`, `TestCrashMidWriteTransactionParent` (mid-effect uncertainty);
- `TestRuntimeCrashBeforeWriteEffectKeepsFileUnchanged`, `TestRuntimeUnreconcilableWriteStopsWithHumanReview` (write boundaries);
- `TestGovernorRollingUsageSurvivesRestart`, `TestGovernorCircuitSurvivesRestart`, `TestGovernorCooldownSurvivesRestart`, `TestGovernorTaskBudgetSurvivesRestart` (governor restart durability);
- `TestReceiptAwareExecutionDebitsExactlyOnePerReceipt`, `TestReceiptAwareExecutionDebitsEveryNewAmplifiedAttemptAndBlocksLane`, `TestReceiptAwareExecutionRejectsAttemptIDReplayWithoutSecondDebit` (receipt chaos at the governor layer);
- `TestLoopResumeContextReachesNewConversation`, `TestLoopResumeConsumesCommittedObservationWithoutReExecution` (recovery with a new conversation);
- `TestProcessRunnerTimeoutKillsFullTree`, `TestProcessRunnerBoundedOutputIndependent`, `TestProcessRunnerTerminatingSignal` (runner-level process chaos).

## Running the suite

```bash
gofmt -l .
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/runstead
bash experiments/protocol/test.sh
```

Focused chaos runs (repeat several times to check determinism):

```bash
go test ./internal/agent/ -run 'Chaos|UnlimitedTextRuntime|UnknownRuntime|ProfileTransition' -count=3
go test ./cmd/runstead/ -run 'CodingLoopCrash' -count=2
```

## Scope boundaries

The suite intentionally does not implement or exercise: browser runtimes
(Camoufox/Playwright/Chromium/CDP/Firefox), Rust helpers, first-party ChatGPT
Web, provider bake-off, model routing, account/session rotation, daemons,
arbitrary shell, new retry systems or automatic account-limit discovery.
M6 remains blocked by #14; the live OmniRoute path remains conditioned on
#29 -> #30 -> #4. This suite proves the behavior with fixtures and fakes.
