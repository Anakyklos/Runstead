package context

import (
	"fmt"
	"sort"
	"strings"

	"github.com/RenyEnnos/Runstead/internal/state"
)

// evidenceSelection is the deterministic evidence projection: every citable
// evidence ID is pinned; observation content lines are degradable newest-first
// and capped by the budget.
type evidenceSelection struct {
	ids        []string // all citable IDs, descending (allocation order)
	content    []degradableLine
	capOmitted []OmittedItem
}

// selectEvidence mirrors the recovery package's verified-progress semantics:
// completed tool attempts and write attempts reconciled as verified completed
// carry citable evidence. All IDs are pinned; content is degradable with
// explicit cap/byte omission records.
func selectEvidence(snapshot *state.RecoverySnapshot, budget Budget) evidenceSelection {
	byID := make(map[string]state.RecoveryEvidence, len(snapshot.Evidence))
	for _, item := range snapshot.Evidence {
		byID[item.EvidenceID] = item
	}
	completed := make([]string, 0, len(snapshot.ToolAttempts))
	for _, attempt := range snapshot.ToolAttempts {
		if attempt.EvidenceID == "" {
			continue
		}
		if attempt.Status == "completed" {
			completed = append(completed, attempt.EvidenceID)
			continue
		}
		if attempt.Status == "reconciled" && attempt.RecoveryReason == "write_effect_completed" {
			completed = append(completed, attempt.EvidenceID)
		}
	}
	// Deterministic newest-first: evidence ID descending (allocation order).
	sort.Strings(completed)
	ids := make([]string, 0, len(completed))
	for index := len(completed) - 1; index >= 0; index-- {
		ids = append(ids, completed[index])
	}

	var content []degradableLine
	var omitted []OmittedItem
	for _, evidenceID := range ids {
		evidence, ok := byID[evidenceID]
		if !ok {
			continue
		}
		line := fmt.Sprintf("evidence %s: %s(%s)", evidence.EvidenceID, evidence.Tool,
			compactJSON(evidence.ArgumentsJSON))
		if data := capChars(compactJSON(evidence.DataJSON), budget.MaxObservationChars); data != "" {
			line += " data: " + data
		}
		if len(content) >= budget.MaxObservationCount {
			omitted = append(omitted, OmittedItem{Kind: FactEvidence, ID: evidence.EvidenceID})
			continue
		}
		content = append(content, degradableLine{text: line, id: evidence.EvidenceID})
	}
	return evidenceSelection{ids: ids, content: content, capOmitted: omitted}
}

// compactJSON removes insignificant whitespace from persisted JSON so one
// observation renders deterministically and compactly. Unparseable input is
// returned verbatim (already sanitized at the persistence boundary).
func compactJSON(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	var buffer strings.Builder
	compact := true
	escaped := false
	for _, char := range value {
		switch {
		case escaped:
			buffer.WriteRune(char)
			escaped = false
		case char == '\\':
			buffer.WriteRune(char)
			escaped = true
		case char == '"':
			buffer.WriteRune(char)
			compact = !compact
		case compact && (char == ' ' || char == '\t' || char == '\n' || char == '\r'):
			// skip insignificant whitespace outside strings
		default:
			buffer.WriteRune(char)
		}
	}
	return buffer.String()
}

// capChars caps a string to max runes with an explicit marker when truncated.
// Truncation here only affects degradable content detail, never pinned IDs.
func capChars(value string, max int) string {
	if max <= 0 || len(value) <= max {
		return value
	}
	if max <= len("...") {
		return value[:max]
	}
	return value[:max-len("...")] + "..."
}
