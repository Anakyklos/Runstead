// Package improvement implements the Issue #55 evidence-backed proposal
// lifecycle: typed, NON-AUTHORITATIVE improvement proposals derived from
// durable task evidence, reviewed by the operator, versioned when applied,
// and measured later through objective validation records.
//
// The package owns the typed contract, the fail-closed state machine and the
// format/target validation. It contains NO execution authority: no execution
// path reads proposals, the model cannot reach the operator commands, and
// nothing here can express policy, governor, verifier, approval or evidence
// semantics. Persistence/provenance validation lives in internal/state.
package improvement

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/RenyEnnos/Runstead/internal/composition"
)

// Kind identifies the concrete, bounded proposal shape.
type Kind string

const (
	// KindComposition proposes a declarative runtime Profile (the M10 typed
	// format) for a project-scoped profile target. The proposed change is a
	// strict Profile JSON document; applying it produces a versioned revision
	// a NEW task may use only through the explicit --profile path.
	KindComposition Kind = "composition"
)

// Status is the explicit fail-closed lifecycle.
type Status string

const (
	StatusPending    Status = "pending"     // proposed; never affects execution
	StatusApproved   Status = "approved"    // operator approval; NOT an applied change
	StatusRejected   Status = "rejected"    // terminal, auditable
	StatusApplied    Status = "applied"     // approved + explicit apply produced a version
	StatusValidated  Status = "validated"   // later objective validation records attached
	StatusRolledBack Status = "rolled_back" // terminal; previous revision restored
)

// Decision is the explicit control-plane review decision.
type Decision string

const (
	DecisionApprove Decision = "approve"
	DecisionReject  Decision = "reject"
)

// Outcome is the operator-attested validation classification.
type Outcome string

const (
	OutcomePositive  Outcome = "positive"
	OutcomeNegative  Outcome = "negative"
	OutcomeUncertain Outcome = "uncertain"
)

var (
	// ErrUnknownProposal wraps every load/transition failure on an absent id.
	ErrUnknownProposal = errors.New("improvement proposal not found")
	// ErrInvalidTransition is the fail-closed lifecycle violation.
	ErrInvalidTransition = errors.New("invalid improvement proposal transition")
	// ErrInvalidProposal covers format/kind/target/scope violations.
	ErrInvalidProposal = errors.New("invalid improvement proposal")
	// ErrInvalidEvidence covers missing or incompatible evidence refs.
	ErrInvalidEvidence = errors.New("invalid improvement proposal evidence")
	// ErrNoBaseRevision covers rollback attempts without a previous revision.
	ErrNoBaseRevision = errors.New("improvement proposal has no base revision to roll back to")
	// ErrEvidenceRevisionMismatch covers evidence produced by a task whose
	// frozen execution contract did not run under the evaluated revision.
	ErrEvidenceRevisionMismatch = errors.New("evidence was not produced under the evaluated proposal revision")
	// ErrImprovementCorrupt covers tampered or inconsistent durable rows
	// (for example a version whose artifact bytes do not match its digest).
	ErrImprovementCorrupt = errors.New("corrupt improvement proposal state")
)

// EvidenceRef is one durable provenance reference.
type EvidenceRef struct {
	TaskID     string `json:"task_id"`
	EvidenceID string `json:"evidence_id"`
}

// ValidationRecord is one later objective observation tied to a version.
type ValidationRecord struct {
	ValidationID string        `json:"validation_id"`
	ProposalID   string        `json:"proposal_id"`
	VersionID    string        `json:"version_id"`
	Outcome      Outcome       `json:"outcome"`
	Evidence     []EvidenceRef `json:"evidence"`
	Notes        string        `json:"notes"`
	ObservedAt   string        `json:"observed_at"`
	CreatedAt    string        `json:"created_at"`
}

// MaterialRef is one exact package selection in the canonical material
// projection. Package metadata is registry-static, so id+version fully
// determine the package material.
type MaterialRef struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

// ProfileMaterial is the canonical, non-secret projection of the
// PROFILE-DETERMINED material of a revision: profile identity, exact package
// selections, declared recipe ids and declared provider id. It is computed
// from the artifact at apply time and re-derivable from a task's frozen
// execution contract, so validation can prove the task ran under EXACTLY
// this revision's material without relying on the profile version string as
// a trust boundary. Optional fields (recipe_ids, provider_id) are included
// only when the artifact declares them; operator seams the Profile does not
// determine (the concrete recipe catalog, the provider endpoint resolution)
// are NOT part of the profile material.
type ProfileMaterial struct {
	ProfileID      string        `json:"profile_id"`
	ProfileVersion string        `json:"profile_version"`
	Packages       []MaterialRef `json:"packages"`
	RecipeIDs      []string      `json:"recipe_ids,omitempty"`
	ProviderID     string        `json:"provider_id,omitempty"`
}

// Canonical renders the deterministic byte form (sorted slices, stable key
// order) used as the digest input.
func (m ProfileMaterial) Canonical() []byte {
	packages := append([]MaterialRef(nil), m.Packages...)
	sort.Slice(packages, func(i, j int) bool {
		if packages[i].ID == packages[j].ID {
			return packages[i].Version < packages[j].Version
		}
		return packages[i].ID < packages[j].ID
	})
	canonical := ProfileMaterial{
		ProfileID:      m.ProfileID,
		ProfileVersion: m.ProfileVersion,
		Packages:       packages,
		RecipeIDs:      sortedStrings(m.RecipeIDs),
		ProviderID:     m.ProviderID,
	}
	data, err := json.Marshal(canonical)
	if err != nil {
		return []byte("{}")
	}
	return data
}

// Digest returns the SHA-256 of the canonical material projection.
func (m ProfileMaterial) Digest() (string, error) {
	data := m.Canonical()
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// ProfileMaterialFromProfile builds the material projection from a parsed
// Profile document (the artifact side of the link).
func ProfileMaterialFromProfile(profile composition.Profile) ProfileMaterial {
	refs := make([]MaterialRef, 0, len(profile.Packages))
	for _, ref := range profile.Packages {
		refs = append(refs, MaterialRef{ID: strings.TrimSpace(ref.ID), Version: strings.TrimSpace(ref.Version)})
	}
	return ProfileMaterial{
		ProfileID:      strings.TrimSpace(profile.ProfileID),
		ProfileVersion: strings.TrimSpace(profile.ProfileVersion),
		Packages:       refs,
		RecipeIDs:      sortedStrings(profile.RecipeIDs),
		ProviderID:     strings.TrimSpace(profile.ProviderID),
	}
}

// ProfileMaterialFromContract rebuilds the task-side projection using the
// stored revision material as the field schema: recipe ids and provider id
// participate only when the revision's artifact declared them; the exact
// package selections and profile identity always participate.
func ProfileMaterialFromContract(profileID, profileVersion string, packageIDs, packageVersions []string, recipeIDs []string, providerID string, schema ProfileMaterial) ProfileMaterial {
	refs := make([]MaterialRef, 0, len(packageIDs))
	for index := range packageIDs {
		refs = append(refs, MaterialRef{ID: strings.TrimSpace(packageIDs[index]), Version: strings.TrimSpace(packageVersions[index])})
	}
	material := ProfileMaterial{
		ProfileID:      strings.TrimSpace(profileID),
		ProfileVersion: strings.TrimSpace(profileVersion),
		Packages:       refs,
	}
	if len(schema.RecipeIDs) > 0 {
		material.RecipeIDs = sortedStrings(recipeIDs)
	}
	if schema.ProviderID != "" {
		material.ProviderID = strings.TrimSpace(providerID)
	}
	return material
}

// Version is one applied revision of a proposal target. ProfileMaterialJSON
// and ProfileMaterialDigest are the canonical PROFILE-DETERMINED material
// projection of the artifact: validation requires a task's frozen contract to
// re-derive EXACTLY this material, so packages/provider/recipe material
// differences fail closed even when the profile version string is identical.
type Version struct {
	VersionID             string `json:"version_id"`
	ProposalID            string `json:"proposal_id"`
	TargetID              string `json:"target_id"`
	Revision              int    `json:"revision"`
	BaseVersionID         string `json:"base_version_id,omitempty"`
	ProfileID             string `json:"profile_id,omitempty"`
	ProfileVersion        string `json:"profile_version,omitempty"`
	ProfileMaterialJSON   string `json:"profile_material_json,omitempty"`
	ProfileMaterialDigest string `json:"profile_material_digest,omitempty"`
	ArtifactDigest        string `json:"artifact_digest"`
	ArtifactJSON          []byte `json:"-"`
	CreatedAt             string `json:"created_at"`
}

// Proposal is the typed, non-authoritative record. ProposedChangeJSON is the
// bounded typed payload (a strict Profile document for KindComposition).
type Proposal struct {
	ProposalID         string
	Kind               Kind
	ScopeID            string
	Title              string
	TargetID           string
	TargetBaseVersion  string
	SourceTaskIDs      []string
	SourceWorkUnitIDs  []string
	ProposedChangeJSON []byte
	Rationale          string
	ExpectedBenefit    string
	InvariantsTouched  []string
	ValidationPlan     []string
	Status             Status
	ReviewDecision     string
	ReviewReason       string
	ReviewedBy         string
	DecidedAt          string
	VersionID          string
	ArtifactPath       string
	RolledBackTo       string
	RolledBackAt       string
	CreatedAt          string
	UpdatedAt          string
}

// Summary is the bounded listing projection.
type Summary struct {
	ProposalID     string
	Kind           Kind
	ScopeID        string
	TargetID       string
	Title          string
	Status         Status
	ReviewDecision string
	VersionID      string
	CreatedAt      string
}

// profileTargetPattern is the ONLY grammar a composition proposal target may
// have. The prefix plus the restricted charset structurally CANNOT express a
// trusted-kernel identity (governor, policy, verifier, evidence, recovery,
// kernel, runtime, provider): there is no field able to name them as targets.
var profileTargetPattern = regexp.MustCompile(`^profiles/[a-zA-Z0-9._-]+$`)

// forbiddenTargetMarkers are explicit additional rejections, kept as a second
// line so a future kind cannot accidentally admit a kernel identity.
var forbiddenTargetMarkers = []string{
	"governor", "policy", "approval", "verifier", "evidence", "recovery",
	"kernel", "runtime", "protocol", "provider", "contract", "task",
}

// ValidateProposal is the pure pre-persist validation: format, kind, target
// grammar, scope, redaction pre-checks and the typed proposed change (strict
// Profile parse for KindComposition). Provenance references are validated by
// the state layer against durable rows.
func ValidateProposal(proposal Proposal) error {
	switch proposal.Kind {
	case KindComposition:
		if err := validateCompositionTarget(proposal.TargetID); err != nil {
			return err
		}
		if len(proposal.ProposedChangeJSON) == 0 {
			return fmt.Errorf("%w: composition proposal requires a Profile document", ErrInvalidProposal)
		}
		if _, err := composition.ParseProfile(proposal.ProposedChangeJSON); err != nil {
			return fmt.Errorf("%w: proposed change is not a strict Profile document: %v", ErrInvalidProposal, err)
		}
	default:
		return fmt.Errorf("%w: unsupported proposal kind %q", ErrInvalidProposal, proposal.Kind)
	}
	if strings.TrimSpace(proposal.ScopeID) == "" {
		return fmt.Errorf("%w: scope_id must not be empty", ErrInvalidProposal)
	}
	if strings.TrimSpace(proposal.Title) == "" {
		return fmt.Errorf("%w: title must not be empty", ErrInvalidProposal)
	}
	// ProposalID is allocated by the durable state layer at propose time, so
	// it is not validated here.
	if proposal.Status != "" && proposal.Status != StatusPending {
		return fmt.Errorf("%w: a new proposal must start pending", ErrInvalidProposal)
	}
	if err := validateStringList(proposal.SourceTaskIDs, "source_task_ids"); err != nil {
		return err
	}
	if err := validateStringList(proposal.SourceWorkUnitIDs, "source_work_unit_ids"); err != nil {
		return err
	}
	if err := validateStringList(proposal.InvariantsTouched, "invariants_touched"); err != nil {
		return err
	}
	if err := validateStringList(proposal.ValidationPlan, "validation_plan"); err != nil {
		return err
	}
	return nil
}

func validateCompositionTarget(target string) error {
	target = strings.TrimSpace(target)
	if !profileTargetPattern.MatchString(target) {
		return fmt.Errorf("%w: composition target %q must match profiles/<name>", ErrInvalidProposal, target)
	}
	lower := strings.ToLower(target)
	for _, marker := range forbiddenTargetMarkers {
		if strings.Contains(lower, marker) {
			return fmt.Errorf("%w: target %q may not reference trusted-kernel authority", ErrInvalidProposal, target)
		}
	}
	return nil
}

func validateStringList(values []string, field string) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			return fmt.Errorf("%w: %s entries must be non-empty", ErrInvalidProposal, field)
		}
		if _, exists := seen[value]; exists {
			return fmt.Errorf("%w: duplicate %s entry %q", ErrInvalidProposal, field, value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

// ValidateEvidenceRefs is a pure structural check; durability is checked by
// state. Refs must be non-empty and without duplicates.
func ValidateEvidenceRefs(refs []EvidenceRef) error {
	if len(refs) == 0 {
		return fmt.Errorf("%w: at least one durable evidence reference is required", ErrInvalidEvidence)
	}
	seen := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		ref.TaskID = strings.TrimSpace(ref.TaskID)
		ref.EvidenceID = strings.TrimSpace(ref.EvidenceID)
		if ref.TaskID == "" || ref.EvidenceID == "" {
			return fmt.Errorf("%w: evidence refs require task_id and evidence_id", ErrInvalidEvidence)
		}
		key := ref.TaskID + "\x00" + ref.EvidenceID
		if _, exists := seen[key]; exists {
			return fmt.Errorf("%w: duplicate evidence ref %q/%q", ErrInvalidEvidence, ref.TaskID, ref.EvidenceID)
		}
		seen[key] = struct{}{}
	}
	return nil
}

// Transition returns the destination status for an apply/validate/rollback
// action, or a typed error when the transition is invalid. approve==apply is
// impossible: apply is a separate explicit operator step.
func (p Proposal) Transition(action string) (Status, error) {
	switch action {
	case "apply":
		if p.Status != StatusApproved {
			return "", fmt.Errorf("%w: apply requires approved (got %s)", ErrInvalidTransition, p.Status)
		}
		return StatusApplied, nil
	case "validate":
		switch p.Status {
		case StatusApplied, StatusValidated:
			return StatusValidated, nil
		}
		return "", fmt.Errorf("%w: validation requires an applied proposal (got %s)", ErrInvalidTransition, p.Status)
	case "rollback":
		switch p.Status {
		case StatusApplied, StatusValidated:
			if p.TargetBaseVersion == "" {
				return "", fmt.Errorf("%w: %v", ErrNoBaseRevision, "proposal has no base revision")
			}
			return StatusRolledBack, nil
		}
		return "", fmt.Errorf("%w: rollback requires an applied proposal (got %s)", ErrInvalidTransition, p.Status)
	default:
		return "", fmt.Errorf("%w: unknown lifecycle action %q", ErrInvalidTransition, action)
	}
}

// ApplyReviewTransition validates an operator decision for the proposal
// status and returns the resulting status.
func (p Proposal) ApplyReviewTransition(decision Decision) (Status, error) {
	switch decision {
	case DecisionApprove:
		if p.Status != StatusPending {
			return "", fmt.Errorf("%w: only a pending proposal can be approved (got %s)", ErrInvalidTransition, p.Status)
		}
		return StatusApproved, nil
	case DecisionReject:
		switch p.Status {
		case StatusPending, StatusApproved:
			return StatusRejected, nil
		}
		return "", fmt.Errorf("%w: only pending/approved proposals can be rejected (got %s)", ErrInvalidTransition, p.Status)
	default:
		return "", fmt.Errorf("%w: unsupported decision %q", ErrInvalidTransition, decision)
	}
}

// MarshalJSONList is a small helper for deterministic JSON arrays.
func MarshalJSONList(values []string) []byte {
	if values == nil {
		values = []string{}
	}
	data, err := json.Marshal(values)
	if err != nil {
		return []byte("[]")
	}
	return data
}

func sortedStrings(values []string) []string {
	copy := append([]string(nil), values...)
	sort.Strings(copy)
	return copy
}
