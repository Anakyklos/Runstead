package improvement

import (
	"errors"
	"strings"
	"testing"
)

func validProposal() Proposal {
	return Proposal{
		Kind:               KindComposition,
		ScopeID:            "proj-a",
		Title:              "add repo.write to coding profile",
		TargetID:           "profiles/coding",
		ProposedChangeJSON: []byte(`{"version":1,"profile_id":"coding","profile_version":"2.0.0","packages":[{"id":"repo.read","version":"1.0.0"},{"id":"repo.write","version":"1.0.0"}]}`),
		Rationale:          "evidence-backed",
		InvariantsTouched:  []string{"policy"},
		ValidationPlan:     []string{"one task per package"},
	}
}

func TestValidateProposalAcceptsComposition(t *testing.T) {
	if err := ValidateProposal(validProposal()); err != nil {
		t.Fatalf("valid composition proposal rejected: %v", err)
	}
}

func TestValidateProposalRejectsTrustedKernelTargets(t *testing.T) {
	for _, target := range []string{
		"governor", "policy/coding", "profiles/verifier", "kernel",
		"runtime", "approval", "provider/main", "profiles/evidence-store",
	} {
		proposal := validProposal()
		if strings.HasPrefix(target, "profiles/") {
			proposal.TargetID = target
		} else {
			proposal.TargetID = "profiles/" + target
		}
		// Some targets violate the grammar outright; some pass the grammar but
		// hit the trusted-kernel marker list. Both must fail closed.
		if err := ValidateProposal(proposal); err == nil {
			t.Fatalf("target %q must be rejected", target)
		}
	}
}

func TestValidateProposalRejectsUnknownKind(t *testing.T) {
	proposal := validProposal()
	proposal.Kind = Kind("anything_else")
	if err := ValidateProposal(proposal); err == nil || !errors.Is(err, ErrInvalidProposal) {
		t.Fatalf("unknown kind error = %v, want ErrInvalidProposal", err)
	}
}

func TestValidateProposalRejectsNonProfileChange(t *testing.T) {
	proposal := validProposal()
	proposal.ProposedChangeJSON = []byte(`{"arbitrary":"blob","callback":"os.system"}`)
	if err := ValidateProposal(proposal); err == nil || !errors.Is(err, ErrInvalidProposal) {
		t.Fatalf("non-Profile change error = %v, want ErrInvalidProposal", err)
	}
}

func TestLifecycleTransitionsFailClosed(t *testing.T) {
	pending := validProposal()
	pending.Status = StatusPending

	// review decisions
	if status, err := pending.ApplyReviewTransition(DecisionApprove); err != nil || status != StatusApproved {
		t.Fatalf("approve = %q/%v, want approved", status, err)
	}
	if status, err := pending.ApplyReviewTransition(DecisionReject); err != nil || status != StatusRejected {
		t.Fatalf("reject = %q/%v, want rejected", status, err)
	}
	if _, err := pending.ApplyReviewTransition(Decision("maybe")); err == nil || !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("unknown decision error = %v, want ErrInvalidTransition", err)
	}

	// approve never equals apply
	approved := validProposal()
	approved.Status = StatusApproved
	if _, err := approved.Transition("apply"); err != nil {
		t.Fatalf("apply from approved = %v", err)
	}
	if _, err := pending.Transition("apply"); err == nil || !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("apply from pending must fail closed, got %v", err)
	}

	// validation requires applied
	applied := approved
	applied.Status = StatusApplied
	applied.TargetBaseVersion = "ver-000001"
	if _, err := applied.Transition("validate"); err != nil {
		t.Fatalf("validate from applied = %v", err)
	}

	// rollback requires a base revision
	first := applied
	first.TargetBaseVersion = ""
	if _, err := first.Transition("rollback"); err == nil || !errors.Is(err, ErrNoBaseRevision) {
		t.Fatalf("rollback without base = %v, want ErrNoBaseRevision", err)
	}
	if _, err := applied.Transition("rollback"); err != nil {
		t.Fatalf("rollback from applied = %v", err)
	}

	// rejected is terminal
	rejected := pending
	rejected.Status = StatusRejected
	if _, err := rejected.ApplyReviewTransition(DecisionApprove); err == nil || !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("approve of rejected must fail closed, got %v", err)
	}
	if _, err := rejected.Transition("apply"); err == nil || !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("apply of rejected must fail closed, got %v", err)
	}
}

func TestValidateEvidenceRefs(t *testing.T) {
	if err := ValidateEvidenceRefs(nil); err == nil || !errors.Is(err, ErrInvalidEvidence) {
		t.Fatalf("empty refs error = %v, want ErrInvalidEvidence", err)
	}
	if err := ValidateEvidenceRefs([]EvidenceRef{{TaskID: "t", EvidenceID: "obs-000001"}, {TaskID: "t", EvidenceID: "obs-000001"}}); err == nil || !errors.Is(err, ErrInvalidEvidence) {
		t.Fatalf("duplicate refs error = %v, want ErrInvalidEvidence", err)
	}
	if err := ValidateEvidenceRefs([]EvidenceRef{{TaskID: "t", EvidenceID: ""}}); err == nil || !errors.Is(err, ErrInvalidEvidence) {
		t.Fatalf("empty evidence id error = %v, want ErrInvalidEvidence", err)
	}
	if err := ValidateEvidenceRefs([]EvidenceRef{{TaskID: "t", EvidenceID: "obs-000001"}}); err != nil {
		t.Fatalf("valid refs rejected: %v", err)
	}
}
