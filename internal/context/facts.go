package context

import (
	"fmt"
	"sort"
	"strings"

	"github.com/RenyEnnos/Runstead/internal/state"
	"github.com/RenyEnnos/Runstead/internal/verifier"
)

// acceptanceState is the deterministic acceptance-check projection. The plan
// checks are authoritative; per-check detail is degradable. A check counts as
// remaining unless the LATEST verification attempt reports it passed.
type acceptanceState struct {
	available    bool
	digest       string
	remaining    []verifier.Check
	failedChecks []string
	detail       []degradableLine
}

// selectAcceptance parses the persisted plan with the strict verifier decoder
// and derives the remaining checks from the latest verification attempt
// (snapshot order is newest first). An unparseable plan renders as explicitly
// unavailable, never as "all passed".
func selectAcceptance(snapshot *state.RecoverySnapshot) acceptanceState {
	result := acceptanceState{available: true}
	if strings.TrimSpace(snapshot.AcceptancePlanSpec) == "" {
		result.available = false
		result.digest = snapshot.AcceptancePlanDigest
		return result
	}
	plan, err := verifier.ParsePlan([]byte(snapshot.AcceptancePlanSpec))
	if err != nil {
		result.available = false
		result.digest = snapshot.AcceptancePlanDigest
		return result
	}
	result.digest = snapshot.AcceptancePlanDigest

	passed := make(map[string]bool)
	if len(snapshot.VerificationAttempts) > 0 {
		latest := snapshot.VerificationAttempts[0] // newest first
		for _, check := range latest.Checks {
			passed[check.CheckID] = check.Status == "passed"
			if check.Status == "failed" {
				result.failedChecks = append(result.failedChecks, check.CheckID)
			}
		}
	}
	for _, check := range plan.Checks {
		if !passed[check.ID] {
			result.remaining = append(result.remaining, check)
			detail := fmt.Sprintf("check %s: %s", check.ID, check.Type)
			if check.Path != "" {
				detail += " path=" + check.Path
			}
			if check.Type == verifier.CheckFileHash {
				detail += " sha256=" + check.SHA256
			}
			if check.Type == verifier.CheckRecipeExitZero {
				detail += " recipe=" + check.Recipe
			}
			result.detail = append(result.detail, degradableLine{text: detail, id: check.ID})
		}
	}
	return result
}

// workspaceFacts is the deterministic workspace-signature projection. Every
// recorded signature is pinned with a freshness classification; per-signature
// detail is degradable.
type workspaceFacts struct {
	pinned string
	detail []degradableLine
	facts  []Fact
}

// selectWorkspaceFacts derives unique recorded workspace signatures from
// accepted actions and classifies each against the current signature known at
// compile time. Classification is presentation only.
func selectWorkspaceFacts(snapshot *state.RecoverySnapshot, current string) workspaceFacts {
	detail := make([]degradableLine, 0, 4)
	seen := make(map[string]int)
	for _, action := range snapshot.Actions {
		if action.WorkspaceSignature != "" {
			seen[action.WorkspaceSignature]++
		}
	}
	if len(seen) == 0 {
		return workspaceFacts{pinned: "workspace signatures: none recorded"}
	}
	facts := make([]Fact, 0, len(seen))
	signatures := make([]string, 0, len(seen))
	for signature := range seen {
		signatures = append(signatures, signature)
	}
	sort.Strings(signatures)

	parts := make([]string, 0, len(signatures))
	for _, signature := range signatures {
		freshness := classifyFreshness(signature, current)
		parts = append(parts, fmt.Sprintf("%s(%s)", signature, freshness))
		line := fmt.Sprintf("signature %s: %d action(s) recorded under this workspace state", signature, seen[signature])
		detail = append(detail, degradableLine{text: line, id: signature})
		facts = append(facts, Fact{
			Kind:      FactWorkspace,
			Origin:    "workspace-signature:" + signature,
			Value:     fmt.Sprintf("%d recorded action(s)", seen[signature]),
			Signature: signature,
			Freshness: freshness,
		})
	}
	return workspaceFacts{
		pinned: "workspace signatures: " + strings.Join(parts, ", "),
		detail: detail,
		facts:  facts,
	}
}

// classifyFreshness classifies a recorded workspace signature against the
// current one. Empty current signature means unknown: facts are presented as
// unverified-current, never silently fresh.
func classifyFreshness(recorded, current string) Freshness {
	switch {
	case current == "":
		return FreshnessUnverifiedCurrent
	case recorded == current:
		return FreshnessCurrent
	default:
		return FreshnessNeedsRefresh
	}
}

// extract builds the deterministic model from the authoritative input: typed
// facts (with provenance), the fixed-order render sections, pinned evidence
// IDs, non-authoritative notes and cap-omission records.
func extract(input Input, budget Budget) model {
	snapshot := input.Snapshot
	var facts []Fact
	notes := make([]Note, 0, len(input.NonAuthoritativeNotes))
	for _, text := range input.NonAuthoritativeNotes {
		notes = append(notes, Note{Text: text})
	}
	var sections []section
	var modelOmitted []OmittedItem

	// Preamble: the authority boundary is structural and explicit.
	sections = append(sections, section{kind: FactObjective, pinned: []string{
		"[reconstruction] deterministic projection of durable Runstead state; the original provider conversation was not retained",
		"AUTHORITATIVE facts below trace to persisted state or environment evidence; NON-AUTHORITATIVE notes are marked and can never satisfy checks.",
	}})

	// Objective.
	objective := strings.TrimSpace(snapshot.Task.Objective)
	if objective == "" {
		objective = "(not recorded)"
	}
	facts = append(facts, Fact{Kind: FactObjective, Origin: snapshot.Task.TaskID, Value: objective})
	sections = append(sections, section{kind: FactObjective, pinned: []string{"objective: " + objective}})

	// Task status / lifecycle.
	status := strings.TrimSpace(snapshot.Task.Status)
	if status == "" {
		status = "(not recorded)"
	}
	facts = append(facts, Fact{Kind: FactStatus, Origin: snapshot.Task.TaskID, Value: status})
	statusLine := fmt.Sprintf("task status: %s", status)
	if snapshot.Task.ResumeCount > 0 {
		statusLine += fmt.Sprintf(" (resume count %d)", snapshot.Task.ResumeCount)
	}
	sections = append(sections, section{kind: FactStatus, pinned: []string{statusLine}})

	// Constraints: consumed loop budgets, rejected repeats.
	rejected := 0
	for _, action := range snapshot.Actions {
		if action.Status == "rejected" {
			rejected++
		}
	}
	constraintValue := fmt.Sprintf("provider turns %d, provider attempts %d, rejected proposals %d",
		len(snapshot.ProviderAttempts), len(snapshot.ProviderAttempts), rejected)
	facts = append(facts, Fact{Kind: FactConstraint, Origin: snapshot.Task.TaskID, Value: constraintValue})
	sections = append(sections, section{kind: FactConstraint, pinned: []string{"constraints: " + constraintValue}})

	// Evidence: all IDs pinned; content degradable.
	evidence := selectEvidence(snapshot, budget)
	for _, evidenceID := range evidence.ids {
		facts = append(facts, Fact{Kind: FactEvidence, Origin: evidenceID, Value: evidenceID})
	}
	modelOmitted = append(modelOmitted, evidence.capOmitted...)
	evidenceSection := section{kind: FactEvidence}
	if len(evidence.ids) > 0 {
		ids := make([]string, len(evidence.ids))
		copy(ids, evidence.ids)
		sort.Strings(ids)
		evidenceSection.pinned = []string{"evidence ids: " + strings.Join(ids, ", ")}
	} else {
		evidenceSection.pinned = []string{"evidence ids: none recorded"}
	}
	evidenceSection.degradable = evidence.content
	sections = append(sections, evidenceSection)

	// Actions and attempts: pinned count line; per-item detail degradable.
	actionCount := len(snapshot.Actions)
	toolCount := len(snapshot.ToolAttempts)
	providerCount := len(snapshot.ProviderAttempts)
	countsValue := fmt.Sprintf("actions %d, tool attempts %d, provider attempts %d", actionCount, toolCount, providerCount)
	facts = append(facts, Fact{Kind: FactConstraint, Origin: snapshot.Task.TaskID, Value: countsValue})
	actionSection := section{kind: FactAction, pinned: []string{"history: " + countsValue}}
	for _, action := range snapshot.Actions {
		facts = append(facts, Fact{Kind: FactAction, Origin: action.ActionID,
			Value: fmt.Sprintf("%s %s", action.Tool, action.Status)})
		actionSection.degradable = append(actionSection.degradable, degradableLine{
			text: fmt.Sprintf("action %s: %s %s", action.ActionID, action.Tool, action.Status),
			id:   action.ActionID,
		})
	}
	sections = append(sections, actionSection)

	// Concrete attempts: typed authoritative facts preserving the
	// action -> attempt -> result/evidence relation (issue #51 review).
	for _, attempt := range snapshot.ToolAttempts {
		value := fmt.Sprintf("tool %s action %s status %s", attempt.Tool, attempt.ActionID, attempt.Status)
		if attempt.EvidenceID != "" {
			value += " evidence " + attempt.EvidenceID
		}
		facts = append(facts, Fact{Kind: FactAttempt, Origin: attempt.ExecutionID, Value: value})
	}
	for _, attempt := range snapshot.ProviderAttempts {
		value := fmt.Sprintf("provider outcome %s", attempt.Outcome)
		if attempt.ClientRequestID != "" {
			value = fmt.Sprintf("provider request %s outcome %s", attempt.ClientRequestID, attempt.Outcome)
		}
		facts = append(facts, Fact{Kind: FactAttempt, Origin: attempt.ExecutionID, Value: value})
	}

	// Failures: all failure IDs pinned; detail degradable (capped).
	failures := selectFailures(snapshot)
	failureSection := section{kind: FactFailure}
	if len(failures.ids) > 0 {
		failureSection.pinned = []string{"unresolved failures: " + strings.Join(failures.ids, ", ")}
	} else {
		failureSection.pinned = []string{"unresolved failures: none"}
	}
	failureSection.degradable = failures.detail
	modelOmitted = append(modelOmitted, failures.capOmitted...)
	for _, rest := range failures.detail[capLimit(len(failures.detail), budget.MaxFailureLines):] {
		modelOmitted = append(modelOmitted, OmittedItem{Kind: FactFailure, ID: rest.id})
	}
	failureSection.degradable = failures.detail[:capLimit(len(failures.detail), budget.MaxFailureLines)]
	sections = append(sections, failureSection)
	for _, failure := range failures.items {
		facts = append(facts, Fact{Kind: FactFailure, Origin: failure.executionID, Value: failure.line})
	}

	// Uncertain effects: all IDs pinned; detail degradable (capped).
	uncertain := selectUncertain(snapshot)
	uncertainSection := section{kind: FactUncertainEffect}
	if len(uncertain.ids) > 0 {
		uncertainSection.pinned = []string{"uncertain effects: " + strings.Join(uncertain.ids, ", ")}
	} else {
		uncertainSection.pinned = []string{"uncertain effects: none"}
	}
	uncertainSection.degradable = uncertain.detail
	modelOmitted = append(modelOmitted, uncertain.capOmitted...)
	for _, rest := range uncertain.detail[capLimit(len(uncertain.detail), budget.MaxUncertainLines):] {
		modelOmitted = append(modelOmitted, OmittedItem{Kind: FactUncertainEffect, ID: rest.id})
	}
	uncertainSection.degradable = uncertain.detail[:capLimit(len(uncertain.detail), budget.MaxUncertainLines)]
	sections = append(sections, uncertainSection)
	for _, item := range uncertain.items {
		facts = append(facts, Fact{Kind: FactUncertainEffect, Origin: item.id, Value: item.line})
	}

	// Pending approvals: all IDs pinned; detail degradable (capped).
	approvalSection := section{kind: FactApproval}
	approvals := input.PendingApprovals
	if len(approvals) > 0 {
		ids := make([]string, 0, len(approvals))
		for _, approval := range approvals {
			ids = append(ids, approval.ActionID)
			facts = append(facts, Fact{Kind: FactApproval, Origin: approval.ActionID,
				Value: fmt.Sprintf("%s %s", approval.Tool, approval.Fingerprint)})
		}
		approvalSection.pinned = []string{"pending approvals: " + strings.Join(ids, ", ")}
		for index, approval := range approvals {
			line := fmt.Sprintf("approval %s: %s", approval.ActionID, approval.Tool)
			if index >= budget.MaxApprovalLines {
				modelOmitted = append(modelOmitted, OmittedItem{Kind: FactApproval, ID: approval.ActionID})
				continue
			}
			approvalSection.degradable = append(approvalSection.degradable, degradableLine{text: line, id: approval.ActionID})
		}
	} else {
		approvalSection.pinned = []string{"pending approvals: none recorded"}
	}
	sections = append(sections, approvalSection)

	// Acceptance checks: remaining check IDs pinned; detail degradable.
	acceptance := selectAcceptance(snapshot)
	acceptanceSection := section{kind: FactAcceptanceCheck}
	switch {
	case !acceptance.available:
		acceptanceSection.pinned = []string{"acceptance plan unavailable (digest " + acceptance.digest + ")"}
	case len(acceptance.remaining) == 0:
		acceptanceSection.pinned = []string{"acceptance checks: none remaining (latest verification passed)"}
	default:
		ids := make([]string, 0, len(acceptance.remaining))
		for _, check := range acceptance.remaining {
			ids = append(ids, check.ID)
			facts = append(facts, Fact{Kind: FactAcceptanceCheck, Origin: acceptance.digest,
				Value: fmt.Sprintf("%s %s", check.ID, check.Type)})
		}
		acceptanceSection.pinned = []string{"remaining acceptance checks: " + strings.Join(ids, ", ")}
		acceptanceSection.degradable = acceptance.detail
	}
	sections = append(sections, acceptanceSection)

	// Verification: latest decision pinned; detail degradable (capped).
	verificationSection := section{kind: FactVerification}
	attempts := snapshot.VerificationAttempts
	if len(attempts) == 0 {
		verificationSection.pinned = []string{"verification: no attempt recorded"}
	} else {
		latest := attempts[0] // newest first
		facts = append(facts, Fact{Kind: FactVerification, Origin: latest.AttemptID,
			Value: latest.Decision + " " + latest.Summary})
		verificationSection.pinned = []string{"verification: latest decision " + latest.Decision}
		detailCount := 0
		for _, attempt := range attempts {
			if detailCount >= budget.MaxVerificationLines {
				modelOmitted = append(modelOmitted, OmittedItem{Kind: FactVerification, ID: attempt.AttemptID})
				continue
			}
			verificationSection.degradable = append(verificationSection.degradable, degradableLine{
				text: fmt.Sprintf("verification %s: %s %s", attempt.AttemptID, attempt.Decision, capChars(attempt.Summary, 256)),
				id:   attempt.AttemptID,
			})
			detailCount++
		}
	}
	sections = append(sections, verificationSection)

	// Workspace facts: the typed facts and the render share the same
	// authority boundary (issue #51 review).
	workspace := selectWorkspaceFacts(snapshot, input.CurrentWorkspaceSignature)
	facts = append(facts, workspace.facts...)
	sections = append(sections, section{kind: FactWorkspace, pinned: []string{workspace.pinned}, degradable: workspace.detail})

	// Non-authoritative section: the marker is pinned; notes degrade by byte
	// budget only (they are navigation, never requirements).
	nonAuthoritativeSection := section{kind: FactObjective}
	if len(notes) == 0 {
		nonAuthoritativeSection.pinned = []string{"NON-AUTHORITATIVE (model-authored summaries/inferences; never facts, never acceptance): none present"}
	} else {
		nonAuthoritativeSection.pinned = []string{"NON-AUTHORITATIVE (model-authored summaries/inferences; never facts, never acceptance):"}
		for _, note := range notes {
			nonAuthoritativeSection.degradable = append(nonAuthoritativeSection.degradable, degradableLine{text: "- " + note.Text})
		}
	}
	sections = append(sections, nonAuthoritativeSection)

	return model{
		facts:       facts,
		notes:       notes,
		evidenceIDs: evidence.ids,
		sections:    sections,
		omitted:     modelOmitted,
	}
}

type failureSelection struct {
	ids        []string
	items      []failureItem
	detail     []degradableLine
	capOmitted []OmittedItem
}

type failureItem struct {
	executionID string
	line        string
}

// selectFailures mirrors the recovery package: deterministic typed tool
// failures in creation order (execution id ascending). All IDs are pinned.
func selectFailures(snapshot *state.RecoverySnapshot) failureSelection {
	var selection failureSelection
	for _, attempt := range snapshot.ToolAttempts {
		if attempt.Status != "failed" {
			continue
		}
		selection.ids = append(selection.ids, attempt.ExecutionID)
		line := fmt.Sprintf("failure %s: %s(%s) %s", attempt.ExecutionID, attempt.Tool,
			compactJSON(attempt.ArgumentsJSON), attempt.Classification)
		selection.items = append(selection.items, failureItem{executionID: attempt.ExecutionID, line: line})
		selection.detail = append(selection.detail, degradableLine{text: line, id: attempt.ExecutionID})
	}
	return selection
}

type uncertainSelection struct {
	ids        []string
	items      []uncertainItem
	detail     []degradableLine
	capOmitted []OmittedItem
}

type uncertainItem struct {
	id   string
	line string
}

// selectUncertain mirrors the recovery package's conservative inventory of
// provider requests that may have reached upstream and interrupted tool
// attempts. All IDs are pinned; detail lines degrade by budget.
func selectUncertain(snapshot *state.RecoverySnapshot) uncertainSelection {
	var selection uncertainSelection
	replaySafe := 0
	writeNotStarted := 0
	for _, attempt := range snapshot.ToolAttempts {
		id := attempt.ExecutionID
		switch attempt.Status {
		case "prepared", "running", "observed", "verified", "uncertain", "verification_failed", "planned":
			if attempt.RecoveryClass == 1 {
				replaySafe++
				continue
			}
			selection.ids = append(selection.ids, id)
			line := fmt.Sprintf("tool %s (%s) was interrupted and cannot be reconciled safely; human review required", attempt.ExecutionID, attempt.Tool)
			selection.items = append(selection.items, uncertainItem{id: id, line: line})
			selection.detail = append(selection.detail, degradableLine{text: line, id: id})
		case "reconciled":
			switch attempt.RecoveryReason {
			case "replay_safe_observation":
				replaySafe++
			case "write_effect_not_started":
				writeNotStarted++
			case "write_effect_unreconcilable":
				selection.ids = append(selection.ids, id)
				line := fmt.Sprintf("tool %s (%s) was interrupted and cannot be reconciled safely; human review required", attempt.ExecutionID, attempt.Tool)
				selection.items = append(selection.items, uncertainItem{id: id, line: line})
				selection.detail = append(selection.detail, degradableLine{text: line, id: id})
			}
		}
	}
	if replaySafe > 0 {
		selection.ids = append(selection.ids, "replay-safe")
		line := fmt.Sprintf("%d prepared observation(s) were reconciled as replay-safe; a re-proposal executes as a new attempt with fresh evidence", replaySafe)
		selection.items = append(selection.items, uncertainItem{id: "replay-safe", line: line})
		selection.detail = append(selection.detail, degradableLine{text: line})
	}
	if writeNotStarted > 0 {
		selection.ids = append(selection.ids, "write-not-started")
		line := fmt.Sprintf("%d write attempt(s) provably never started and were reconciled; a re-proposal executes as a new attempt with a fresh before-hash", writeNotStarted)
		selection.items = append(selection.items, uncertainItem{id: "write-not-started", line: line})
		selection.detail = append(selection.detail, degradableLine{text: line})
	}
	for _, attempt := range snapshot.ProviderAttempts {
		id := attempt.ExecutionID
		switch attempt.Status {
		case "prepared", "running", "uncertain", "planned":
			selection.ids = append(selection.ids, id)
			line := fmt.Sprintf("provider request %s (execution %s) may have reached upstream; outcome unknown; conservative debit preserved; not retried",
				attempt.ClientRequestID, attempt.ExecutionID)
			selection.items = append(selection.items, uncertainItem{id: id, line: line})
			selection.detail = append(selection.detail, degradableLine{text: line, id: id})
		case "reconciled":
			if attempt.Uncertain {
				selection.ids = append(selection.ids, id)
				line := fmt.Sprintf("provider request %s (execution %s) may have reached upstream; outcome unknown; conservative debit preserved; not retried",
					attempt.ClientRequestID, attempt.ExecutionID)
				selection.items = append(selection.items, uncertainItem{id: id, line: line})
				selection.detail = append(selection.detail, degradableLine{text: line, id: id})
			}
		}
	}
	return selection
}
