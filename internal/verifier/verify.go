package verifier

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/RenyEnnos/Runstead/internal/recipe"
	"github.com/RenyEnnos/Runstead/internal/state"
	"github.com/RenyEnnos/Runstead/internal/tools"
)

// Structural check ids are stable and rendered in inspect. They are not
// operator-chosen; they exist for every verification attempt.
const (
	checkEvidenceGrounded    = "evidence_grounded"
	checkEvidenceClaimsTyped = "evidence_claims_typed"
	checkNoUncertainAttempt  = "no_uncertain_attempts"
	checkNoPendingApproval   = "no_pending_approvals"
	checkWritesReconciled    = "writes_reconciled"
	checkGitObserved         = "git_observed"
	checkAcceptanceCriteria  = "acceptance_criteria_required"
	checkBudgetExceeded      = "check_budget_exceeded"
)

// Verify evaluates one completion proposal against authoritative state and
// returns the structured report. It performs no SQLite transaction and no
// process execution; all external observation goes through the Observer.
func (v *Verifier) Verify(input Input) Report {
	now := input.Now
	if strings.TrimSpace(now) == "" {
		now = verifyNow()
	}
	report := Report{
		TaskID:    input.TaskID,
		Decision:  DecisionPassed,
		CreatedAt: now,
	}
	if v == nil || v.observer == nil {
		report.Decision = DecisionBlocked
		report.Summary = "verification unavailable: no authoritative observer"
		report.Checks = []CheckResult{{
			ID: checkEvidenceGrounded, Type: "structural", Status: CheckBlocked,
			Reason: "verification has no authoritative observer; completion is refused",
		}}
		return report
	}

	var uncertain []CheckResult
	var blocked []CheckResult
	var failed []CheckResult
	var passed []CheckResult
	appendResult := func(result CheckResult) {
		// Limits bound the report content: every description is truncated to
		// MaxObservedChars so a hostile or enormous environment can never blow
		// up the report (issue #11 review).
		result.Expected = boundText(v.limits.MaxObservedChars, result.Expected)
		result.Observed = boundText(v.limits.MaxObservedChars, result.Observed)
		result.Reason = boundText(v.limits.MaxObservedChars, result.Reason)
		switch result.Status {
		case CheckUncertain:
			uncertain = append(uncertain, result)
		case CheckBlocked:
			blocked = append(blocked, result)
		case CheckFailed:
			failed = append(failed, result)
		default:
			passed = append(passed, result)
		}
		report.Checks = append(report.Checks, result)
	}

	// 1. Evidence grounding: every cited evidence ID must exist in the task's
	// persisted evidence. A fabricated or foreign ID is a failed check.
	report.CitedEvidence = resolveCitedEvidence(input)
	grounding := CheckResult{ID: checkEvidenceGrounded, Type: "structural", Status: CheckPassed}
	var missing []string
	for _, cited := range report.CitedEvidence {
		if !cited.Exists {
			missing = append(missing, cited.EvidenceID)
		}
	}
	if len(missing) > 0 {
		grounding.Status = CheckFailed
		grounding.Expected = "every cited evidence id exists in the task's persisted evidence"
		grounding.Observed = "missing: " + strings.Join(missing, ",")
		grounding.Reason = "evidence_ids_not_found"
	}
	appendResult(grounding)

	// 1b. Claimed evidence types: the final response must declare the tool that
	// produced each evidence it cites, and that claim must match the persisted
	// tool. A type-incompatible citation is a failed check: a read_file
	// observation can never support a run_recipe claim (issue #11 review).
	typing := CheckResult{ID: checkEvidenceClaimsTyped, Type: "structural", Status: CheckPassed}
	var mismatched []string
	for _, cited := range report.CitedEvidence {
		if cited.Exists && !cited.ToolMatches {
			mismatched = append(mismatched, fmt.Sprintf("%s(claimed=%s,persisted=%s)", cited.EvidenceID, cited.ClaimedTool, cited.Tool))
		}
	}
	if len(mismatched) > 0 {
		typing.Status = CheckFailed
		typing.Expected = "every cited evidence's claimed tool matches its persisted tool"
		typing.Observed = "type mismatch: " + strings.Join(mismatched, ",")
		typing.Reason = "evidence_type_mismatch"
	}
	appendResult(typing)

	// 2. Uncertain/human-review attempts: completion is refused while any
	// authoritative effect is uncertain.
	uncertainRefs := uncertainAttempts(input.ToolAttempts)
	if len(uncertainRefs) > 0 {
		appendResult(CheckResult{
			ID: checkNoUncertainAttempt, Type: "structural", Status: CheckUncertain,
			Expected: "no uncertain or interrupted effect",
			Observed: fmt.Sprintf("%d uncertain attempt(s)", len(uncertainRefs)),
			Reason:   "uncertain_effect_blocks_completion",
		})
		report.UncertainAttempts = uncertainRefs
	} else {
		appendResult(CheckResult{ID: checkNoUncertainAttempt, Type: "structural", Status: CheckPassed})
	}

	// 3. Pending operator approvals block completion.
	if len(input.PendingApprovals) > 0 {
		appendResult(CheckResult{
			ID: checkNoPendingApproval, Type: "structural", Status: CheckBlocked,
			Expected: "no pending operator approval",
			Observed: fmt.Sprintf("%d pending approval(s)", len(input.PendingApprovals)),
			Reason:   "approval_required_pending",
		})
		report.PendingApprovals = append([]string(nil), input.PendingApprovals...)
	} else {
		appendResult(CheckResult{ID: checkNoPendingApproval, Type: "structural", Status: CheckPassed})
	}

	// 4. Write evidence reconciled against the real filesystem: a write the
	// task persisted must match the current authoritative file state.
	reconciled := v.reconcileWrites(input)
	report.WriteReconciliation = reconciled
	writeResult := CheckResult{ID: checkWritesReconciled, Type: "structural", Status: CheckPassed}
	var mismatches []string
	for _, item := range reconciled {
		if !item.Matches {
			mismatches = append(mismatches, item.Path)
		}
	}
	if len(mismatches) > 0 {
		writeResult.Status = CheckFailed
		writeResult.Expected = "every persisted write matches the current filesystem"
		writeResult.Observed = "mismatch: " + strings.Join(mismatches, ",")
		writeResult.Reason = "write_evidence_does_not_match_filesystem"
	}
	appendResult(writeResult)

	// 5. Real git observation with change attribution. Git attribution is a
	// "where practical" observation: an unavailable git repository does not
	// block completion, but the limitation is recorded honestly in the report
	// and surfaced in inspect.
	git := v.observeGit(input)
	report.Git = git
	gitResult := CheckResult{ID: checkGitObserved, Type: "structural", Status: CheckPassed}
	if !git.Available {
		gitResult.Observed = "unavailable: " + git.Failure
		report.Limitations = append(report.Limitations, boundText(v.limits.MaxObservedChars, "git observation unavailable: "+git.Failure))
	}
	if git.BaselineTruncated {
		// The bounded baseline captured at task start was truncated. Pre-existing
		// changes outside the truncated baseline window can be attributed as
		// during_task; the limitation is recorded in the report (persisted with
		// the attempt) and surfaced in inspect, never silently ignored.
		gitResult.Observed = "baseline truncated at task start; pre-existing changes outside the truncated baseline may be attributed as during_task"
		report.Limitations = append(report.Limitations, "git baseline truncated at task start; pre-existing changes outside the truncated baseline window may be attributed as during_task")
	}
	appendResult(gitResult)

	// 5b. Acceptance criteria fail closed: completion is refused blocked when no
	// operator acceptance plan with at least one check exists. Without a
	// task-specific acceptance criterion, a completion proposal can never be
	// proven against the task objective (issue #11 review). This is a
	// control-plane dependency the model cannot satisfy; the task stays
	// durably resumable for the operator to supply a plan.
	if len(v.planChecks()) == 0 {
		appendResult(CheckResult{
			ID: checkAcceptanceCriteria, Type: "structural", Status: CheckBlocked,
			Expected: "an operator acceptance plan with at least one acceptance check",
			Observed: "no acceptance plan configured for the task",
			Reason:   "acceptance_plan_missing",
		})
	}

	// 6. Truncation is recorded explicitly; it is never silently ignored. A
	// check that depends on truncated content fails (see recipe_exit_zero with
	// require_untruncated), but truncation alone does not fail every check.
	report.TruncatedEvidence = truncatedRecipeEvidence(input.Evidence)

	// 7. Operator acceptance checks over authoritative state. Limits.MaxChecks
	// caps the checks evaluated in one attempt so an enormous operator plan
	// can never blow up the report; when the budget is exhausted the overflow
	// is a blocked control-plane check (completion cannot be proven).
	budgetExceeded := false
	for _, check := range v.planChecks() {
		if len(report.Checks) >= v.limits.MaxChecks {
			budgetExceeded = true
			break
		}
		appendResult(v.evaluateCheck(check, input))
	}
	if budgetExceeded {
		appendResult(CheckResult{
			ID: checkBudgetExceeded, Type: "structural", Status: CheckBlocked,
			Expected: fmt.Sprintf("at most %d checks evaluated per attempt", v.limits.MaxChecks),
			Observed: "the operator plan has more checks than the attempt budget",
			Reason:   "check_budget_exceeded",
		})
	}

	// Combine: uncertain wins (authoritative), then blocked (control-plane),
	// then failed (returnable to execution).
	switch {
	case len(uncertain) > 0:
		report.Decision = DecisionUncertain
		report.Summary = "completion refused: an authoritative effect is uncertain"
	case len(blocked) > 0:
		report.Decision = DecisionBlocked
		report.Summary = "completion refused: a control-plane dependency is pending"
	case len(failed) > 0:
		report.Decision = DecisionFailed
		report.Summary = fmt.Sprintf("completion refused: %d check(s) failed", len(failed))
	default:
		report.Decision = DecisionPassed
		// The verified summary is produced by the verifier from the acceptance
		// checks, never from model prose (issue #11 review). It names the
		// operator checks that passed so the completion report is traceable to
		// acceptance criteria.
		var checkNames []string
		for _, check := range passed {
			if check.Type == "structural" {
				continue
			}
			checkNames = append(checkNames, check.ID)
		}
		switch len(checkNames) {
		case 0:
			report.Summary = "completion verified: all acceptance checks passed"
		case 1:
			report.Summary = "completion verified: acceptance check passed (" + checkNames[0] + ")"
		default:
			report.Summary = fmt.Sprintf("completion verified: %d acceptance checks passed (%s)", len(checkNames), strings.Join(checkNames, ","))
		}
		report.Summary = boundText(v.limits.MaxObservedChars, report.Summary)
	}
	return report
}

// resolveCitedEvidence resolves every typed evidence citation against the
// task's persisted evidence: each cited id must exist AND its claimed tool
// must match the persisted tool (issue #11 review).
func resolveCitedEvidence(input Input) []CitedEvidence {
	cited := make([]CitedEvidence, 0, len(input.FinalEvidence))
	byID := make(map[string]state.RecoveryEvidence, len(input.Evidence))
	for _, item := range input.Evidence {
		byID[item.EvidenceID] = item
	}
	claims := append([]EvidenceClaim(nil), input.FinalEvidence...)
	sort.Slice(claims, func(i, j int) bool { return claims[i].EvidenceID < claims[j].EvidenceID })
	for _, claim := range claims {
		item, ok := byID[claim.EvidenceID]
		entry := CitedEvidence{EvidenceID: claim.EvidenceID, ClaimedTool: claim.Tool, Exists: ok}
		if ok {
			entry.Tool = item.Tool
			entry.ToolMatches = item.Tool == claim.Tool
		}
		cited = append(cited, entry)
	}
	return cited
}

// uncertainAttempts returns the authoritative tool attempts whose status
// blocks completion (prepared, uncertain, human_review_required).
func uncertainAttempts(attempts []state.RecoveryToolAttempt) []AttemptRef {
	var refs []AttemptRef
	for _, attempt := range attempts {
		switch attempt.Status {
		case "prepared", "uncertain", "human_review_required":
			refs = append(refs, AttemptRef{
				ExecutionID: attempt.ExecutionID,
				Tool:        attempt.Tool,
				Status:      attempt.Status,
				Reason:      attempt.RecoveryReason,
			})
		}
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].ExecutionID < refs[j].ExecutionID })
	return refs
}

// reconcileWrites compares every persisted write evidence against the current
// filesystem through the observer.
func (v *Verifier) reconcileWrites(input Input) []WriteReconciled {
	var reconciled []WriteReconciled
	for _, item := range input.Evidence {
		if item.Tool != tools.ToolWriteFile && item.Tool != tools.ToolApplyPatch {
			continue
		}
		var write tools.WriteEvidence
		if err := json.Unmarshal([]byte(item.DataJSON), &write); err != nil {
			continue
		}
		if write.Path == "" || write.AfterHash == "" {
			continue
		}
		entry := WriteReconciled{
			Path:       write.Path,
			ChangeKind: write.ChangeKind,
			AfterHash:  write.AfterHash,
			EvidenceID: item.EvidenceID,
		}
		observed, present, failure := v.observer.FileSHA256(write.Path)
		switch {
		case failure != nil:
			entry.ObservedHash = "unreadable"
		case !present:
			entry.ObservedHash = "absent"
		default:
			entry.ObservedHash = observed
		}
		if write.ChangeKind == "created" {
			entry.Matches = present && observed == write.AfterHash
		} else {
			entry.Matches = present && observed == write.AfterHash
		}
		reconciled = append(reconciled, entry)
	}
	sort.Slice(reconciled, func(i, j int) bool { return reconciled[i].Path < reconciled[j].Path })
	return reconciled
}

// observeGit captures the authoritative git state and attributes changes
// between the task-start baseline and now.
func (v *Verifier) observeGit(input Input) *GitObservation {
	observation := &GitObservation{}
	currentStatus, statusTruncated, statusFailure := v.observer.GitStatusText()
	if statusFailure != nil {
		observation.Failure = failureText(statusFailure)
		return observation
	}
	observation.CurrentStatus = currentStatus
	observation.Truncated = statusTruncated
	if diff, diffTruncated, diffFailure := v.observer.GitDiffText(); diffFailure == nil {
		observation.CurrentDiff = diff
		observation.Truncated = observation.Truncated || diffTruncated
	} else {
		observation.Truncated = true
	}
	observation.Available = true
	observation.BaselineTruncated = input.BaselineGitStatusTruncated || input.BaselineGitDiffTruncated

	baseline := parseGitStatus(input.BaselineGitStatus)
	current := parseGitStatus(currentStatus)
	observation.PreExisting = changedFiles(baseline, nil, v.limits.MaxChangedFiles)
	observation.DuringTask = changedFiles(current, baseline, v.limits.MaxChangedFiles)
	return observation
}

// failureText renders a typed observer failure without raw model content.
func failureText(failure error) string {
	if failure == nil {
		return ""
	}
	return failure.Error()
}

// parseGitStatus parses `git status --short` output into changed files. Each
// line is "<XY> <path>"; with --no-renames and core.quotepath=false the path
// is the rest of the line after the two status columns and one space.
func parseGitStatus(output string) map[string]string {
	files := make(map[string]string)
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		if len(line) < 3 {
			continue
		}
		status := line[:2]
		path := strings.TrimPrefix(line[3:], " ")
		if path == "" || path == "." {
			continue
		}
		files[path] = status
	}
	return files
}

// changedFiles returns the deterministic sorted changed-file list: the
// current set minus the baseline (same path with a different status counts as
// changed during the task).
func changedFiles(current, baseline map[string]string, limit int) []ChangedFile {
	paths := make(map[string]string)
	for path, status := range current {
		if baseStatus, existed := baseline[path]; !existed || baseStatus != status {
			paths[path] = status
		}
	}
	ordered := make([]string, 0, len(paths))
	for path := range paths {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	if limit > 0 && len(ordered) > limit {
		ordered = ordered[:limit]
	}
	files := make([]ChangedFile, 0, len(ordered))
	for _, path := range ordered {
		files = append(files, ChangedFile{Path: path, Status: paths[path]})
	}
	return files
}

// truncatedRecipeEvidence lists the persisted recipe evidence ids whose
// output was truncated.
func truncatedRecipeEvidence(evidence []state.RecoveryEvidence) []string {
	var truncated []string
	for _, item := range evidence {
		if item.Tool != tools.ToolRunRecipe {
			continue
		}
		var process recipe.Evidence
		if err := json.Unmarshal([]byte(item.DataJSON), &process); err != nil {
			continue
		}
		if process.StdoutTruncated || process.StderrTruncated {
			truncated = append(truncated, item.EvidenceID)
		}
	}
	sort.Strings(truncated)
	return truncated
}

// evaluateCheck evaluates one operator acceptance check against authoritative
// state.
func (v *Verifier) evaluateCheck(check Check, input Input) CheckResult {
	result := CheckResult{ID: check.ID, Type: string(check.Type)}
	switch check.Type {
	case CheckFileExists, CheckFileAbsent, CheckFileHash:
		hash, present, failure := v.observer.FileSHA256(check.Path)
		switch {
		case failure != nil:
			result.Status = CheckBlocked
			result.Reason = "file_observation_failed"
			result.Observed = failureText(failure)
		case check.Type == CheckFileExists:
			result.Expected = "file exists at " + check.Path
			if present {
				result.Status = CheckPassed
				result.Observed = "present"
			} else {
				result.Status = CheckFailed
				result.Observed = "absent"
				result.Reason = "file_not_found"
			}
		case check.Type == CheckFileAbsent:
			result.Expected = "file absent at " + check.Path
			if !present {
				result.Status = CheckPassed
				result.Observed = "absent"
			} else {
				result.Status = CheckFailed
				result.Observed = "present (hash " + shortHash(hash) + ")"
				result.Reason = "file_present"
			}
		case check.Type == CheckFileHash:
			result.Expected = "file at " + check.Path + " has sha256 " + shortHash(check.SHA256)
			if !present {
				result.Status = CheckFailed
				result.Observed = "absent"
				result.Reason = "file_not_found"
			} else if hash == check.SHA256 {
				result.Status = CheckPassed
				result.Observed = "hash matches"
			} else {
				result.Status = CheckFailed
				result.Observed = "hash " + shortHash(hash)
				result.Reason = "hash_mismatch"
			}
		}
	case CheckRecipeExitZero:
		result.Expected = "a run_recipe evidence for " + check.Recipe + " with exit code 0"
		evidence, found := recipeEvidenceFor(input.Evidence, check.Recipe)
		if !found {
			result.Status = CheckFailed
			result.Observed = "no executed recipe evidence for " + check.Recipe
			result.Reason = "recipe_evidence_missing"
			break
		}
		result.EvidenceIDs = evidenceIDs(evidence)
		process := latestRecipeEvidence(evidence)
		truncated := process.StdoutTruncated || process.StderrTruncated
		if check.RequireUntruncated && truncated {
			result.Status = CheckFailed
			result.Observed = fmt.Sprintf("exit=%d truncated=yes (evidence %s)", process.ExitCode, process.EvidenceID)
			result.Reason = "truncated_output_cannot_support_conclusion"
			break
		}
		if !process.Started {
			result.Status = CheckFailed
			result.Observed = "process did not start"
			result.Reason = "recipe_start_failed"
			break
		}
		if process.TimedOut {
			result.Status = CheckFailed
			result.Observed = "timed out"
			result.Reason = "recipe_timed_out"
			break
		}
		if process.Canceled {
			result.Status = CheckFailed
			result.Observed = "canceled"
			result.Reason = "recipe_canceled"
			break
		}
		if process.Signal != "" {
			result.Status = CheckFailed
			result.Observed = "signal " + process.Signal
			result.Reason = "recipe_terminated_by_signal"
			break
		}
		if process.ExitCode != 0 {
			result.Status = CheckFailed
			result.Observed = fmt.Sprintf("exit=%d", process.ExitCode)
			result.Reason = "recipe_exit_nonzero"
			break
		}
		result.Status = CheckPassed
		result.Observed = "exit=0"
		if truncated {
			result.Observed += " (output truncated, recorded)"
		}
	}
	return result
}

func shortHash(value string) string {
	if len(value) <= 12 {
		return value
	}
	return value[:12]
}

// boundText truncates a description to at most limit characters. A truncation
// marker makes the bound explicit so a bounded report is never confused with
// a complete one. Limits.MaxObservedChars bounds one expected/observed/reason
// description (issue #11 review: declared limits must be enforced).
func boundText(limit int, value string) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	return value[:limit] + "[truncated]"
}

// recipeEvidenceFor returns every executed recipe evidence for the id. A
// recipe execution is only citable when it came from the #26 boundary with a
// real process that started; evidence rows exist only for successful
// observations (tool_results), so the boundary is already enforced.
func recipeEvidenceFor(evidence []state.RecoveryEvidence, recipeID string) ([]recipe.Evidence, bool) {
	var found []recipe.Evidence
	for _, item := range evidence {
		if item.Tool != tools.ToolRunRecipe {
			continue
		}
		var process recipe.Evidence
		if err := json.Unmarshal([]byte(item.DataJSON), &process); err != nil {
			continue
		}
		if process.RecipeID != recipeID {
			continue
		}
		found = append(found, process)
	}
	return found, len(found) > 0
}

func evidenceIDs(evidence []recipe.Evidence) []string {
	ids := make([]string, 0, len(evidence))
	for _, item := range evidence {
		if item.EvidenceID != "" {
			ids = append(ids, item.EvidenceID)
		}
	}
	sort.Strings(ids)
	return ids
}

// latestRecipeEvidence returns the most recently executed evidence for a
// recipe (by evidence id order).
func latestRecipeEvidence(evidence []recipe.Evidence) recipe.Evidence {
	latest := evidence[0]
	for _, item := range evidence[1:] {
		if item.EvidenceID > latest.EvidenceID {
			latest = item
		}
	}
	return latest
}
