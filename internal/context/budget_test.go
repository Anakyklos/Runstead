package context

import (
	"testing"

	"github.com/RenyEnnos/Runstead/internal/state"
)

// TestDefaultBudgetValues pins the deterministic budget used by the CLI: any
// change is an explicit, reviewable decision.
func TestDefaultBudgetValues(t *testing.T) {
	budget := DefaultBudget()
	expected := Budget{
		MaxContextBytes:      32 << 10,
		MaxObservationCount:  8,
		MaxObservationChars:  4 << 10,
		MaxFailureLines:      32,
		MaxUncertainLines:    16,
		MaxApprovalLines:     16,
		MaxVerificationLines: 8,
	}
	if budget != expected {
		t.Fatalf("DefaultBudget() = %+v, want %+v", budget, expected)
	}
}

// TestZeroBudgetFallsBackToDefault mirrors the recovery package contract: a
// zero Budget means "use DefaultBudget".
func TestZeroBudgetFallsBackToDefault(t *testing.T) {
	compiled, err := (&Compiler{}).Compile(Input{
		Snapshot: &state.RecoverySnapshot{},
		Budget:   Budget{},
	})
	if err != nil {
		t.Fatalf("Compile with zero budget: %v", err)
	}
	if compiled.Diagnostics.Budget != DefaultBudget() {
		t.Fatalf("zero budget did not fall back to defaults: %+v", compiled.Diagnostics.Budget)
	}
}

// TestBudgetFieldValidation rejects a budget with a non-positive context
// ceiling when explicit (the zero value is the documented fallback).
func TestBudgetFieldValidation(t *testing.T) {
	_, err := (&Compiler{}).Compile(Input{
		Snapshot: &state.RecoverySnapshot{Task: state.RecoveryTask{Objective: "o"}},
		Budget:   Budget{MaxContextBytes: -1},
	})
	if err == nil {
		t.Fatal("negative explicit budget did not fail closed")
	}
}
