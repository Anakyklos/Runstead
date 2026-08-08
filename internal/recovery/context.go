package recovery

import (
	"fmt"
	"sort"
	"strings"

	"github.com/RenyEnnos/Runstead/internal/state"
)

// Budget bounds the reconstructed model-facing recovery context (issue #9).
// The budget is deterministic: required evidence IDs, unresolved failures and
// uncertain attempts are always retained, while historical observation content
// is selected newest-first and capped, and the whole context is hard-truncated
// to MaxContextBytes with an explicit marker. Irrelevant historic noise is
// dropped by construction; evidence required to justify completion is never
// silently discarded (all evidence IDs survive).
type Budget struct {
	// MaxContextBytes caps the total rendered context.
	MaxContextBytes int
	// MaxObservationCount is the number of observations whose content is
	// rendered (newest first). All evidence IDs are always listed regardless.
	MaxObservationCount int
	// MaxObservationChars caps one observation content rendering.
	MaxObservationChars int
	// MaxFailureLines caps the number of unresolved failure lines.
	MaxFailureLines int
	// MaxUncertainLines caps the number of uncertain attempt lines.
	MaxUncertainLines int
}

// DefaultBudget is the recovery context budget used by the CLI.
func DefaultBudget() Budget {
	return Budget{
		MaxContextBytes:     32 << 10,
		MaxObservationCount: 8,
		MaxObservationChars: 4 << 10,
		MaxFailureLines:     32,
		MaxUncertainLines:   16,
	}
}

// Context is the bounded model-facing reconstruction summary.
type Context struct {
	// Text is the rendered context appended to the transcript under the
	// recovery role.
	Text string
	// EvidenceIDs are the citable evidence IDs available for grounding.
	EvidenceIDs []string
	// Chars is the rendered length in bytes; it is always <= Budget.
	Chars int
}

// BuildContext renders a deterministic, bounded recovery summary from the
// persisted snapshot. The summary is built exclusively from already-sanitized
// persisted state and is re-sanitized before return.
func BuildContext(snapshot *state.RecoverySnapshot, budget Budget) Context {
	if budget.MaxContextBytes <= 0 {
		budget = DefaultBudget()
	}
	var builder strings.Builder
	builder.WriteString("This task was interrupted and is being resumed from durable Runstead state. ")
	builder.WriteString("The original provider conversation was not retained; continue from this summary. ")
	builder.WriteString("Do not repeat completed actions. Do not re-issue provider requests that may have reached the provider. ")
	builder.WriteString("New observations must be freshly executed; an old action fingerprint is never a reason to reuse an old result.\n\n")

	objective := strings.TrimSpace(snapshot.Task.Objective)
	if objective == "" {
		objective = "(not recorded)"
	}
	fmt.Fprintf(&builder, "Objective: %s\n", objective)
	workspace := strings.TrimSpace(snapshot.Task.Workspace)
	if workspace == "" {
		workspace = "(not recorded)"
	}
	fmt.Fprintf(&builder, "Workspace: %s\n\n", workspace)

	// Verified progress: completed observations with citable evidence. The
	// newest observations are rendered with content; every evidence ID is
	// always listed.
	progress := verifiedProgress(snapshot)
	if len(progress) > 0 {
		builder.WriteString("Verified progress:\n")
		rendered := 0
		for _, item := range progress {
			if rendered >= budget.MaxObservationCount {
				builder.WriteString(fmt.Sprintf("- %d further completed observations are omitted (evidence IDs below)\n", len(progress)-rendered))
				break
			}
			fmt.Fprintf(&builder, "- %s: %s(%s)\n", item.EvidenceID, item.Tool, item.Arguments)
			if content := capped(item.Content, budget.MaxObservationChars); content != "" {
				fmt.Fprintf(&builder, "  evidence data: %s\n", content)
			}
			rendered++
		}
		builder.WriteString("\n")
	}

	// Unresolved failures: deterministic typed tool failures that remain open.
	failures := unresolvedFailures(snapshot)
	if len(failures) > 0 {
		builder.WriteString("Unresolved failures:\n")
		count := 0
		for _, failure := range failures {
			if count >= budget.MaxFailureLines {
				builder.WriteString(fmt.Sprintf("- %d further failures are omitted\n", len(failures)-count))
				break
			}
			fmt.Fprintf(&builder, "- %s(%s) failed: %s\n", failure.Tool, failure.Arguments, failure.Classification)
			count++
		}
		builder.WriteString("\n")
	}

	// Uncertain attempts: provider requests that may have reached upstream and
	// interrupted tool attempts. These remain represented in recovery evidence
	// and are never silently retried.
	uncertain := uncertainAttempts(snapshot)
	if len(uncertain) > 0 {
		builder.WriteString("Uncertain attempts:\n")
		count := 0
		for _, item := range uncertain {
			if count >= budget.MaxUncertainLines {
				builder.WriteString(fmt.Sprintf("- %d further uncertain attempts are omitted\n", len(uncertain)-count))
				break
			}
			builder.WriteString("- " + item + "\n")
			count++
		}
		builder.WriteString("\n")
	}

	// Available evidence: every citable ID survives so a grounded final can
	// reference historical observations without re-executing them.
	if len(progress) > 0 {
		ids := make([]string, 0, len(progress))
		for _, item := range progress {
			ids = append(ids, item.EvidenceID)
		}
		builder.WriteString("Available evidence: " + strings.Join(ids, ", ") + "\n\n")
	}

	// Current constraints: consumed loop budgets so the resumed run honors the
	// same control boundaries.
	fmt.Fprintf(&builder, "Constraints: %d provider turns and %d provider attempts were consumed before interruption; %d repeated proposals were rejected.\n",
		len(snapshot.ProviderAttempts), len(snapshot.ProviderAttempts), rejectedCount(snapshot))

	text := truncate(builder.String(), budget.MaxContextBytes)
	text = state.Redact(text)
	return Context{
		Text:        text,
		EvidenceIDs: evidenceIDs(progress),
		Chars:       len(text),
	}
}

type progressItem struct {
	EvidenceID string
	Tool       string
	Arguments  string
	Content    string
}

type failureItem struct {
	Tool           string
	Arguments      string
	Classification string
}

// verifiedProgress returns completed observations with citable evidence in
// deterministic newest-first order (by evidence ID descending, which follows
// the run's allocation order).
func verifiedProgress(snapshot *state.RecoverySnapshot) []progressItem {
	byID := make(map[string]state.RecoveryEvidence, len(snapshot.Evidence))
	for _, item := range snapshot.Evidence {
		byID[item.EvidenceID] = item
	}
	completed := make(map[string]bool, len(snapshot.ToolAttempts))
	for _, attempt := range snapshot.ToolAttempts {
		// Completed attempts and write attempts reconciled as verified
		// completed carry citable evidence.
		if attempt.EvidenceID == "" {
			continue
		}
		if attempt.Status == "completed" {
			completed[attempt.EvidenceID] = true
			continue
		}
		if attempt.Status == "reconciled" && attempt.RecoveryReason == "write_effect_completed" {
			completed[attempt.EvidenceID] = true
		}
	}
	items := make([]progressItem, 0, len(completed))
	for evidenceID := range completed {
		evidence, ok := byID[evidenceID]
		if !ok {
			continue
		}
		items = append(items, progressItem{
			EvidenceID: evidence.EvidenceID,
			Tool:       evidence.Tool,
			Arguments:  compactJSON(evidence.ArgumentsJSON),
			Content:    compactJSON(evidence.DataJSON),
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].EvidenceID > items[j].EvidenceID })
	return items
}

// unresolvedFailures returns deterministic typed tool failures in creation
// order (execution id ascending).
func unresolvedFailures(snapshot *state.RecoverySnapshot) []failureItem {
	var items []failureItem
	for _, attempt := range snapshot.ToolAttempts {
		if attempt.Status != "failed" {
			continue
		}
		items = append(items, failureItem{
			Tool:           attempt.Tool,
			Arguments:      compactJSON(attempt.ArgumentsJSON),
			Classification: attempt.Classification,
		})
	}
	return items
}

// uncertainAttempts returns deterministic human-readable lines for provider
// requests that may have reached upstream and interrupted tool attempts,
// including reconciled attempts whose meaning the model must understand
// (replay-safe reads, writes that never started, conservative provider
// debits).
func uncertainAttempts(snapshot *state.RecoverySnapshot) []string {
	var lines []string
	replaySafe := 0
	writeNotStarted := 0
	for _, attempt := range snapshot.ToolAttempts {
		switch attempt.Status {
		case "prepared", "running", "observed", "verified", "uncertain", "verification_failed", "planned":
			if attempt.RecoveryClass == 1 {
				replaySafe++
			} else {
				lines = append(lines, fmt.Sprintf("tool %s (%s) was interrupted and cannot be reconciled safely; human review required",
					attempt.ExecutionID, attempt.Tool))
			}
		case "reconciled":
			switch attempt.RecoveryReason {
			case "replay_safe_observation":
				replaySafe++
			case "write_effect_not_started":
				writeNotStarted++
			case "write_effect_completed":
				// Verified progress; rendered in the verified-progress section.
			case "write_effect_unreconcilable":
				lines = append(lines, fmt.Sprintf("tool %s (%s) was interrupted and cannot be reconciled safely; human review required",
					attempt.ExecutionID, attempt.Tool))
			}
		}
	}
	if replaySafe > 0 {
		lines = append(lines, fmt.Sprintf("%d prepared observation(s) were reconciled as replay-safe; a re-proposal executes as a new attempt with fresh evidence",
			replaySafe))
	}
	if writeNotStarted > 0 {
		lines = append(lines, fmt.Sprintf("%d write attempt(s) provably never started and were reconciled; a re-proposal executes as a new attempt with a fresh before-hash",
			writeNotStarted))
	}
	for _, attempt := range snapshot.ProviderAttempts {
		switch attempt.Status {
		case "prepared", "running", "uncertain", "planned":
			lines = append(lines, fmt.Sprintf("provider request %s (execution %s) may have reached upstream; outcome unknown; conservative debit preserved; not retried",
				attempt.ClientRequestID, attempt.ExecutionID))
		case "reconciled":
			if attempt.Uncertain {
				lines = append(lines, fmt.Sprintf("provider request %s (execution %s) may have reached upstream; outcome unknown; conservative debit preserved; not retried",
					attempt.ClientRequestID, attempt.ExecutionID))
			}
		}
	}
	return lines
}

func rejectedCount(snapshot *state.RecoverySnapshot) int {
	count := 0
	for _, action := range snapshot.Actions {
		if action.Status == "rejected" {
			count++
		}
	}
	return count
}

func evidenceIDs(items []progressItem) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.EvidenceID)
	}
	return ids
}

// compactJSON renders a stored JSON document as a single compact line.
func compactJSON(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" || raw == "null" {
		return ""
	}
	return raw
}

// capped truncates value deterministically to at most max bytes.
func capped(value string, max int) string {
	if max <= 0 || len(value) <= max {
		return value
	}
	if max <= len("...") {
		return value[:max]
	}
	return value[:max-len("...")] + "..."
}

// truncate hard-truncates the rendered context to at most max bytes with an
// explicit marker so the budget is never exceeded.
func truncate(value string, max int) string {
	if max <= 0 || len(value) <= max {
		return value
	}
	marker := "\n...[recovery context truncated to budget]"
	if max <= len(marker) {
		return value[:max]
	}
	return value[:max-len(marker)] + marker
}
