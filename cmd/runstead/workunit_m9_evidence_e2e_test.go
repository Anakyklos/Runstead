package main

// M9 evidence-gate E2E extension (issue #53): four independent read-only
// Work Units through the REAL governed loop under the ceiling concurrency=4,
// proving at the composition-root level that the scheduler lets four unit
// loops run concurrently while every provider attempt still flows through the
// ONE real governor-owned executor (max physical flight == 1), every attempt
// is accounted exactly once, evidence ids stay unique and per-row
// work_unit_id provenance is exact. This is the ceiling-bound E2E companion
// of the issue #109 acceptance test (which proved the same contract at
// concurrency=2) and feeds the M9 evidence report.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/agent"
	"github.com/RenyEnnos/Runstead/internal/policy"
	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/state"
	"github.com/RenyEnnos/Runstead/internal/tools"
	"github.com/RenyEnnos/Runstead/internal/verifier"
	"github.com/RenyEnnos/Runstead/internal/workunit"
)

// TestWorkUnitM9FourConcurrentReadOnlyGovernedE2E proves the M9
// governor-constrained scenario (D) at the ceiling: concurrency=4 with four
// independent read-only units through real loops. Deterministic regardless of
// governor queue interleaving (per-unit keyed scripted responses); all
// counting assertions are exact.
func TestWorkUnitM9FourConcurrentReadOnlyGovernedE2E(t *testing.T) {
	workspace := t.TempDir()
	for _, file := range []string{"a.txt", "b.txt", "c.txt", "d.txt"} {
		if err := os.WriteFile(filepath.Join(workspace, file), []byte(file+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := verifier.ParsePlan([]byte(wuAcceptedPlan))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	store := openWorkUnitStore(t, stateDir)
	ctx := context.Background()
	taskID := "wu-m9-four"
	registry, err := tools.NewRegistry(tools.Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.BootstrapTask(ctx, store, state.TaskRecord{
		TaskID: taskID, Objective: "parent", Workspace: workspace, Model: "scripted",
		ConfigJSON: wuRealConfigSnapshot(t, registry, plan),
	}, plan, registry); err != nil {
		t.Fatal(err)
	}
	definitions := []workunit.Definition{
		{WorkUnitID: "wu-a", Objective: "read a.txt", Tools: []string{"read_file"}, AcceptancePlan: []byte(wuAcceptedPlan)},
		{WorkUnitID: "wu-b", Objective: "list the workspace", Tools: []string{"list_files"}, AcceptancePlan: []byte(wuAcceptedPlan)},
		{WorkUnitID: "wu-c", Objective: "search the workspace", Tools: []string{"search_text"}, AcceptancePlan: []byte(wuAcceptedPlan)},
		{WorkUnitID: "wu-d", Objective: "read d.txt", Tools: []string{"read_file"}, AcceptancePlan: []byte(wuAcceptedPlan)},
	}

	// Per-unit keyed responses; each final cites the unit's OWN latest
	// persisted evidence id, so the interleaving through the governor's
	// serialized lane cannot affect the outcome.
	client := &evidenceAwareKeyedClient{
		db:     openReviewDB(t, stateDir),
		taskID: taskID,
		queues: map[string][]string{
			taskID + "-wu-a": {
				`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
				`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"a done","evidence":[{"evidence_id":"@@EVIDENCE@@","tool":"read_file"}]}</runstead_final>`,
			},
			taskID + "-wu-b": {
				`<runstead_action>{"version":"runstead.protocol.v1","tool":"list_files","arguments":{"path":"."}}</runstead_action>`,
				`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"b done","evidence":[{"evidence_id":"@@EVIDENCE@@","tool":"list_files"}]}</runstead_final>`,
			},
			taskID + "-wu-c": {
				`<runstead_action>{"version":"runstead.protocol.v1","tool":"search_text","arguments":{"query":"txt","path":"."}}</runstead_action>`,
				`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"c done","evidence":[{"evidence_id":"@@EVIDENCE@@","tool":"search_text"}]}</runstead_final>`,
			},
			taskID + "-wu-d": {
				`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"d.txt"}}</runstead_action>`,
				`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"d done","evidence":[{"evidence_id":"@@EVIDENCE@@","tool":"read_file"}]}</runstead_final>`,
			},
			taskID + "-": {
				`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"c.txt"}}</runstead_action>`,
				`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"parent done","evidence":[{"evidence_id":"@@EVIDENCE@@","tool":"read_file"}]}</runstead_final>`,
			},
		},
	}
	executor := newPersistentScriptedExecutorFor(t, store, client)
	emptySeed := &agent.RecoverySeed{}
	pieces := unitLoopPieces{
		runner:           executor,
		registry:         registry,
		model:            "scripted",
		providerIdentity: provider.Identity{},
		trace:            func(agent.TraceLine) {},
		store:            store,
		policy:           policy.NewStatic(policy.Config{}, nil),
		limits:           agent.DefaultLimits(),
		recovery:         emptySeed,
	}
	chainErr := runWorkUnitChain(ctx, store, taskID, workspace, registry, definitions, 4,
		func(ctx context.Context, unit state.WorkUnit) (workunit.RunResult, error) {
			return runUnitLoop(ctx, pieces, taskID, unit)
		})
	if chainErr != nil {
		t.Fatalf("chain: %v", chainErr)
	}
	// All four units must be durably completed before the parent loop may
	// run (chain gate closed).
	for _, id := range []string{"wu-a", "wu-b", "wu-c", "wu-d"} {
		unit, err := store.GetWorkUnit(ctx, taskID, id)
		if err != nil {
			t.Fatal(err)
		}
		if unit.Status != "completed" {
			t.Fatalf("%s = %s, want completed", id, unit.Status)
		}
	}

	// Parent loop through the same governed executor.
	parentLoop, err := agent.NewLoop(agent.Config{
		Runner:               executor,
		Registry:             registry,
		Limits:               agent.DefaultLimits(),
		Model:                "scripted",
		ProviderIdentity:     provider.Identity{},
		Trace:                func(agent.TraceLine) {},
		State:                store,
		Policy:               policy.NewStatic(policy.Config{}, nil),
		Verifier:             verifier.New(registry, plan),
		AcceptancePlanDigest: plan.Digest(),
		Recovery:             emptySeed,
	})
	if err != nil {
		t.Fatal(err)
	}
	parentResult := parentLoop.Run(ctx, agent.Task{ID: taskID, Prompt: "parent"})
	if parentResult.Outcome != agent.OutcomeCompleted {
		t.Fatalf("parent outcome = %s (%s)", parentResult.Outcome, parentResult.StopReason)
	}

	// Exactly-once accounting: 2 turns per unit (4 units) + 2 parent turns.
	if got := workUnitRowCount(t, stateDir, `SELECT COUNT(*) FROM provider_attempts WHERE task_id = ?`, taskID); got != 10 {
		t.Fatalf("provider attempts = %d, want 10 (4 units x 2 + parent x 2)", got)
	}
	// The physical lane stayed serialized: the scheduler never bypassed the
	// governor to force provider parallelism even at the ceiling bound.
	if client.maxFlight != 1 {
		t.Fatalf("concurrent provider Completes = %d, want 1 (governor MaxInFlight stays authoritative)", client.maxFlight)
	}
	// Evidence ids unique under concurrency: 4 unit reads + 1 parent read.
	db := openReviewDB(t, stateDir)
	var evidenceTotal, evidenceDistinct int
	if err := db.QueryRow(`SELECT COUNT(*), COUNT(DISTINCT evidence_id) FROM tool_results WHERE task_id = ?`, taskID).Scan(&evidenceTotal, &evidenceDistinct); err != nil {
		t.Fatal(err)
	}
	if evidenceTotal != 5 || evidenceDistinct != 5 {
		t.Fatalf("evidence = %d total / %d distinct, want 5/5 (unique under concurrency)", evidenceTotal, evidenceDistinct)
	}
	// Provenance: every row class carries the correct owning unit; the
	// parent's rows are task-level ('').
	for _, table := range []string{"actions", "tool_attempts", "provider_attempts", "verification_attempts"} {
		var orphan int
		if err := db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE task_id = ? AND work_unit_id NOT IN ('wu-a','wu-b','wu-c','wu-d','')`, taskID).Scan(&orphan); err != nil {
			t.Fatal(err)
		}
		if orphan != 0 {
			t.Fatalf("%s rows with wrong work_unit_id = %d", table, orphan)
		}
		var countA, countB, countC, countD, countTask int
		if err := db.QueryRow(`SELECT
			SUM(CASE WHEN work_unit_id='wu-a' THEN 1 ELSE 0 END),
			SUM(CASE WHEN work_unit_id='wu-b' THEN 1 ELSE 0 END),
			SUM(CASE WHEN work_unit_id='wu-c' THEN 1 ELSE 0 END),
			SUM(CASE WHEN work_unit_id='wu-d' THEN 1 ELSE 0 END),
			SUM(CASE WHEN work_unit_id='' THEN 1 ELSE 0 END)
			FROM `+table+` WHERE task_id = ?`, taskID).Scan(&countA, &countB, &countC, &countD, &countTask); err != nil {
			t.Fatal(err)
		}
		if countA == 0 || countB == 0 || countC == 0 || countD == 0 || countTask == 0 {
			t.Fatalf("%s provenance = a:%d b:%d c:%d d:%d task:%d, want all non-zero", table, countA, countB, countC, countD, countTask)
		}
	}
	// Governor accounting: every attempt debited exactly once.
	var misDebited int
	if err := db.QueryRow(`SELECT COUNT(*) FROM provider_attempts WHERE task_id = ? AND attempt_debited != 1`, taskID).Scan(&misDebited); err != nil {
		t.Fatal(err)
	}
	if misDebited != 0 {
		t.Fatalf("under/over-debited attempts = %d", misDebited)
	}
}
