package main

// Issue #55: the runstead improvement control plane. Every subcommand is an
// EXPLICIT operator action; no protocol tool exists for any of them, so the
// model can never propose-in, approve, apply, validate or roll back its own
// changes. Proposals are non-authoritative information: nothing in the
// execution path reads these tables, and applying a proposal only produces a
// versioned declarative artifact the operator may point a NEW task at.
//
// Subcommands parse arguments manually (like decide/resume) so flags may
// appear before or after the positional id, and unknown flags or missing
// values fail closed instead of being silently ignored.

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/RenyEnnos/Runstead/internal/improvement"
	"github.com/RenyEnnos/Runstead/internal/state"
)

const (
	improvementReviewerDefault = "operator"
	improvementTimeFormat      = time.RFC3339
)

// improvementValueFlags are the flags that consume a value.
var improvementValueFlags = map[string]bool{
	"--state-dir": true, "--kind": true, "--scope": true, "--title": true,
	"--target": true, "--base": true, "--change": true, "--rationale": true,
	"--expected-benefit": true, "--source-task": true, "--source-workunit": true,
	"--evidence": true, "--invariant": true, "--validation-plan": true,
	"--decision": true, "--reason": true, "--reviewer": true, "--output": true,
	"--outcome": true, "--notes": true, "--status": true,
}

// improvementBoolFlags are boolean flags (no value).
var improvementBoolFlags = map[string]bool{"--artifact": true}

// parseImprovementArgs parses subcommand arguments manually. Flags may appear
// before or after positionals (the flag package stops at the first
// positional, which would silently drop later flags). Unknown flags and
// missing values fail closed.
func parseImprovementArgs(args []string) (values map[string]string, positionals []string, err error) {
	values = make(map[string]string)
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if !strings.HasPrefix(arg, "-") {
			positionals = append(positionals, arg)
			continue
		}
		name := arg
		value := ""
		hasInline := false
		if eq := strings.Index(arg, "="); eq >= 0 {
			name = arg[:eq]
			value = arg[eq+1:]
			hasInline = true
		}
		if improvementBoolFlags[name] {
			if hasInline {
				return nil, nil, fmt.Errorf("%s does not take a value", name)
			}
			values[name] = "true"
			continue
		}
		if !improvementValueFlags[name] {
			return nil, nil, fmt.Errorf("unknown flag %q", arg)
		}
		if !hasInline {
			if index+1 >= len(args) {
				return nil, nil, fmt.Errorf("%s requires a value", name)
			}
			index++
			value = args[index]
		}
		values[name] = value
	}
	return values, positionals, nil
}

func improvementCommand(ctx context.Context, args []string, out, errOut io.Writer) int {
	if len(args) == 0 || hasHelp(args) {
		printImprovementHelp(out)
		return exitSuccess
	}
	sub := args[0]
	switch sub {
	case "propose":
		return improvementProposeCommand(ctx, args[1:], out, errOut)
	case "list":
		return improvementListCommand(ctx, args[1:], out, errOut)
	case "show":
		return improvementShowCommand(ctx, args[1:], out, errOut)
	case "review":
		return improvementReviewCommand(ctx, args[1:], out, errOut)
	case "apply":
		return improvementApplyCommand(ctx, args[1:], out, errOut)
	case "validate":
		return improvementValidateCommand(ctx, args[1:], out, errOut)
	case "rollback":
		return improvementRollbackCommand(ctx, args[1:], out, errOut)
	default:
		fmt.Fprintf(errOut, "improvement: unknown subcommand %q\n", sub)
		printImprovementHelp(errOut)
		return exitUsage
	}
}

func improvementStoreDir(values map[string]string) (string, int) {
	dir, err := resolveStateDir(values["--state-dir"], values["--state-dir"] != "")
	if err != nil {
		return "", exitUsage
	}
	return dir, exitSuccess
}

func improvementNow() string { return time.Now().UTC().Format(improvementTimeFormat) }

// improvementProposeCommand persists a pending, non-authoritative proposal.
// Provenance (source tasks, work units, evidence) is validated against
// durable state; a proposal can never cite evidence that does not exist.
func improvementProposeCommand(ctx context.Context, args []string, out, errOut io.Writer) int {
	values, positionals, err := parseImprovementArgs(args)
	if err != nil {
		fmt.Fprintf(errOut, "improvement propose: %v\n", err)
		return exitUsage
	}
	if len(positionals) != 0 {
		fmt.Fprintln(errOut, "improvement propose: no positional arguments are accepted")
		return exitUsage
	}
	changeFile := strings.TrimSpace(values["--change"])
	change, err := os.ReadFile(changeFile)
	if err != nil {
		fmt.Fprintf(errOut, "improvement propose: cannot read change file: %v\n", err)
		return exitUsage
	}
	refs, err := parseEvidenceRefs(values["--evidence"])
	if err != nil {
		fmt.Fprintf(errOut, "improvement propose: %v\n", err)
		return exitUsage
	}
	proposal := improvement.Proposal{
		Kind:               improvement.Kind(strings.TrimSpace(values["--kind"])),
		ScopeID:            strings.TrimSpace(values["--scope"]),
		Title:              strings.TrimSpace(values["--title"]),
		TargetID:           strings.TrimSpace(values["--target"]),
		TargetBaseVersion:  strings.TrimSpace(values["--base"]),
		SourceTaskIDs:      splitCSV(values["--source-task"]),
		SourceWorkUnitIDs:  splitCSV(values["--source-workunit"]),
		ProposedChangeJSON: change,
		Rationale:          values["--rationale"],
		ExpectedBenefit:    values["--expected-benefit"],
		InvariantsTouched:  splitCSV(values["--invariant"]),
		ValidationPlan:     splitCSV(values["--validation-plan"]),
	}
	dir, code := improvementStoreDir(values)
	if code != exitSuccess {
		fmt.Fprintf(errOut, "improvement propose: invalid state dir\n")
		return code
	}
	store, err := openStore(dir)
	if err != nil {
		fmt.Fprintf(errOut, "improvement propose: state unavailable: %v\n", err)
		return exitUnavailable
	}
	defer store.Close()
	stored, err := store.ProposeImprovement(ctx, proposal, refs)
	if err != nil {
		fmt.Fprintf(errOut, "improvement propose: %v\n", err)
		return exitUsage
	}
	fmt.Fprintf(out, "proposal: %s status: pending scope: %s target: %s\n",
		stored.ProposalID, stored.ScopeID, stored.TargetID)
	return exitSuccess
}

func improvementListCommand(ctx context.Context, args []string, out, errOut io.Writer) int {
	values, positionals, err := parseImprovementArgs(args)
	if err != nil {
		fmt.Fprintf(errOut, "improvement list: %v\n", err)
		return exitUsage
	}
	if len(positionals) != 0 {
		fmt.Fprintln(errOut, "improvement list: no positional arguments are accepted")
		return exitUsage
	}
	dir, code := improvementStoreDir(values)
	if code != exitSuccess {
		fmt.Fprintf(errOut, "improvement list: invalid state dir\n")
		return code
	}
	store, err := openStore(dir)
	if err != nil {
		fmt.Fprintf(errOut, "improvement list: state unavailable: %v\n", err)
		return exitUnavailable
	}
	defer store.Close()
	summaries, err := store.ListImprovements(ctx, values["--scope"], values["--status"])
	if err != nil {
		fmt.Fprintf(errOut, "improvement list: %v\n", err)
		return exitUnavailable
	}
	fmt.Fprintln(out, "ID\tKIND\tSCOPE\tTARGET\tSTATUS\tTITLE")
	for _, summary := range summaries {
		fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\t%s\n", summary.ProposalID, summary.Kind, summary.ScopeID,
			summary.TargetID, summary.Status, summary.Title)
	}
	return exitSuccess
}

func improvementShowCommand(ctx context.Context, args []string, out, errOut io.Writer) int {
	values, positionals, err := parseImprovementArgs(args)
	if err != nil {
		fmt.Fprintf(errOut, "improvement show: %v\n", err)
		return exitUsage
	}
	if len(positionals) != 1 {
		fmt.Fprintln(errOut, "improvement show: exactly one proposal id is required")
		return exitUsage
	}
	proposalID := positionals[0]
	dir, code := improvementStoreDir(values)
	if code != exitSuccess {
		fmt.Fprintf(errOut, "improvement show: invalid state dir\n")
		return code
	}
	store, err := openStore(dir)
	if err != nil {
		fmt.Fprintf(errOut, "improvement show: state unavailable: %v\n", err)
		return exitUnavailable
	}
	defer store.Close()
	proposal, err := store.LoadImprovement(ctx, proposalID)
	if err != nil {
		fmt.Fprintf(errOut, "improvement show: %v\n", err)
		return exitNotFound
	}
	if values["--artifact"] == "true" {
		return showImprovementArtifact(ctx, store, proposal, out, errOut)
	}
	fmt.Fprintf(out, "proposal: %s\n", proposal.ProposalID)
	fmt.Fprintf(out, "kind: %s scope: %s target: %s\n", proposal.Kind, proposal.ScopeID, proposal.TargetID)
	fmt.Fprintf(out, "title: %s\n", proposal.Title)
	fmt.Fprintf(out, "status: %s", proposal.Status)
	if proposal.VersionID != "" {
		fmt.Fprintf(out, " version: %s", proposal.VersionID)
	}
	if proposal.RolledBackTo != "" {
		fmt.Fprintf(out, " rolled_back_to: %s", proposal.RolledBackTo)
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "source_tasks: %s\n", strings.Join(proposal.SourceTaskIDs, ","))
	fmt.Fprintf(out, "source_work_units: %s\n", strings.Join(proposal.SourceWorkUnitIDs, ","))
	refs, err := store.LoadImprovementEvidence(ctx, proposalID)
	if err != nil {
		fmt.Fprintf(errOut, "improvement show: %v\n", err)
		return exitCorrupt
	}
	fmt.Fprintf(out, "evidence: %s\n", renderEvidenceRefs(refs))
	fmt.Fprintf(out, "base_version: %s\n", proposal.TargetBaseVersion)
	if proposal.ReviewDecision != "" {
		fmt.Fprintf(out, "review: %s by %s at %s reason: %s\n", proposal.ReviewDecision,
			proposal.ReviewedBy, proposal.DecidedAt, proposal.ReviewReason)
	}
	fmt.Fprintf(out, "rationale: %s\n", proposal.Rationale)
	fmt.Fprintf(out, "expected_benefit: %s\n", proposal.ExpectedBenefit)
	fmt.Fprintf(out, "invariants_touched: %s\n", strings.Join(proposal.InvariantsTouched, ","))
	fmt.Fprintf(out, "validation_plan: %s\n", strings.Join(proposal.ValidationPlan, ","))
	validations, err := store.LoadImprovementValidations(ctx, proposalID)
	if err != nil {
		fmt.Fprintf(errOut, "improvement show: %v\n", err)
		return exitCorrupt
	}
	for _, record := range validations {
		fmt.Fprintf(out, "validation: %s outcome: %s evidence: %s at %s notes: %s\n",
			record.ValidationID, record.Outcome, renderEvidenceRefs(record.Evidence), record.ObservedAt, record.Notes)
	}
	fmt.Fprintf(out, "created: %s updated: %s artifact_path: %s\n", proposal.CreatedAt, proposal.UpdatedAt, proposal.ArtifactPath)
	return exitSuccess
}

func showImprovementArtifact(ctx context.Context, store *state.Store, proposal improvement.Proposal, out, errOut io.Writer) int {
	var versionID string
	switch proposal.Status {
	case improvement.StatusApplied, improvement.StatusValidated:
		versionID = proposal.VersionID
	case improvement.StatusRolledBack:
		versionID = proposal.RolledBackTo
	default:
		fmt.Fprintln(errOut, "improvement show: no revision artifact exists for a non-applied proposal")
		return exitUsage
	}
	version, err := store.LoadImprovementVersion(ctx, versionID)
	if err != nil {
		fmt.Fprintf(errOut, "improvement show: %v\n", err)
		return exitCorrupt
	}
	if _, err := out.Write(version.ArtifactJSON); err != nil {
		fmt.Fprintf(errOut, "improvement show: %v\n", err)
		return exitCorrupt
	}
	fmt.Fprintln(out)
	return exitSuccess
}

func improvementReviewCommand(ctx context.Context, args []string, out, errOut io.Writer) int {
	values, positionals, err := parseImprovementArgs(args)
	if err != nil {
		fmt.Fprintf(errOut, "improvement review: %v\n", err)
		return exitUsage
	}
	if len(positionals) != 1 {
		fmt.Fprintln(errOut, "improvement review: exactly one proposal id is required")
		return exitUsage
	}
	dir, code := improvementStoreDir(values)
	if code != exitSuccess {
		fmt.Fprintf(errOut, "improvement review: invalid state dir\n")
		return code
	}
	store, err := openStore(dir)
	if err != nil {
		fmt.Fprintf(errOut, "improvement review: state unavailable: %v\n", err)
		return exitUnavailable
	}
	defer store.Close()
	reviewer := values["--reviewer"]
	if strings.TrimSpace(reviewer) == "" {
		reviewer = improvementReviewerDefault
	}
	if err := store.ReviewImprovement(ctx, positionals[0], improvement.Decision(values["--decision"]), values["--reason"], reviewer, improvementNow()); err != nil {
		fmt.Fprintf(errOut, "improvement review: %v\n", err)
		return exitUsage
	}
	fmt.Fprintf(out, "proposal: %s reviewed: %s\n", positionals[0], values["--decision"])
	return exitSuccess
}

func improvementApplyCommand(ctx context.Context, args []string, out, errOut io.Writer) int {
	values, positionals, err := parseImprovementArgs(args)
	if err != nil {
		fmt.Fprintf(errOut, "improvement apply: %v\n", err)
		return exitUsage
	}
	if len(positionals) != 1 {
		fmt.Fprintln(errOut, "improvement apply: exactly one proposal id is required")
		return exitUsage
	}
	output := strings.TrimSpace(values["--output"])
	if output == "" {
		fmt.Fprintln(errOut, "improvement apply: --output path is required")
		return exitUsage
	}
	dir, code := improvementStoreDir(values)
	if code != exitSuccess {
		fmt.Fprintf(errOut, "improvement apply: invalid state dir\n")
		return code
	}
	store, err := openStore(dir)
	if err != nil {
		fmt.Fprintf(errOut, "improvement apply: state unavailable: %v\n", err)
		return exitUnavailable
	}
	defer store.Close()
	version, err := store.ApplyImprovement(ctx, positionals[0], output, improvementNow())
	if err != nil {
		fmt.Fprintf(errOut, "improvement apply: %v\n", err)
		return exitUsage
	}
	// The artifact file is a projection of the durable revision bytes. The DB
	// transaction already committed; a failed write is reported and the byte
	// projection stays recoverable via `improvement show --artifact`.
	if err := writeFileAtomic(output, version.ArtifactJSON); err != nil {
		fmt.Fprintf(errOut, "improvement apply: revision %s stored, but materializing %s failed: %v (recover with: runstead improvement show %s --artifact)\n",
			version.VersionID, output, err, positionals[0])
		return exitUnavailable
	}
	fmt.Fprintf(out, "proposal: %s applied version: %s revision: %d digest: %s\n",
		positionals[0], version.VersionID, version.Revision, version.ArtifactDigest)
	return exitSuccess
}

func improvementValidateCommand(ctx context.Context, args []string, out, errOut io.Writer) int {
	values, positionals, err := parseImprovementArgs(args)
	if err != nil {
		fmt.Fprintf(errOut, "improvement validate: %v\n", err)
		return exitUsage
	}
	if len(positionals) != 1 {
		fmt.Fprintln(errOut, "improvement validate: exactly one proposal id is required")
		return exitUsage
	}
	refs, err := parseEvidenceRefs(values["--evidence"])
	if err != nil {
		fmt.Fprintf(errOut, "improvement validate: %v\n", err)
		return exitUsage
	}
	dir, code := improvementStoreDir(values)
	if code != exitSuccess {
		fmt.Fprintf(errOut, "improvement validate: invalid state dir\n")
		return code
	}
	store, err := openStore(dir)
	if err != nil {
		fmt.Fprintf(errOut, "improvement validate: state unavailable: %v\n", err)
		return exitUnavailable
	}
	defer store.Close()
	record, err := store.ValidateImprovement(ctx, positionals[0], improvement.Outcome(values["--outcome"]), refs, values["--notes"], improvementNow())
	if err != nil {
		fmt.Fprintf(errOut, "improvement validate: %v\n", err)
		return exitUsage
	}
	fmt.Fprintf(out, "proposal: %s validation: %s outcome: %s\n", positionals[0], record.ValidationID, record.Outcome)
	return exitSuccess
}

func improvementRollbackCommand(ctx context.Context, args []string, out, errOut io.Writer) int {
	values, positionals, err := parseImprovementArgs(args)
	if err != nil {
		fmt.Fprintf(errOut, "improvement rollback: %v\n", err)
		return exitUsage
	}
	if len(positionals) != 1 {
		fmt.Fprintln(errOut, "improvement rollback: exactly one proposal id is required")
		return exitUsage
	}
	dir, code := improvementStoreDir(values)
	if code != exitSuccess {
		fmt.Fprintf(errOut, "improvement rollback: invalid state dir\n")
		return code
	}
	store, err := openStore(dir)
	if err != nil {
		fmt.Fprintf(errOut, "improvement rollback: state unavailable: %v\n", err)
		return exitUnavailable
	}
	defer store.Close()
	artifact, err := store.RollbackImprovement(ctx, positionals[0], values["--reason"], improvementNow())
	if err != nil {
		fmt.Fprintf(errOut, "improvement rollback: %v\n", err)
		return exitUsage
	}
	// Rewrite the materialized projection with the restored revision bytes.
	proposal, loadErr := store.LoadImprovement(ctx, positionals[0])
	if loadErr != nil {
		fmt.Fprintf(errOut, "improvement rollback: %v\n", loadErr)
		return exitCorrupt
	}
	if proposal.ArtifactPath != "" {
		if writeErr := writeFileAtomic(proposal.ArtifactPath, artifact); writeErr != nil {
			fmt.Fprintf(errOut, "improvement rollback: restored revision %s, but rewriting %s failed: %v\n",
				proposal.RolledBackTo, proposal.ArtifactPath, writeErr)
			return exitUnavailable
		}
	}
	fmt.Fprintf(out, "proposal: %s rolled_back_to: %s\n", positionals[0], proposal.RolledBackTo)
	return exitSuccess
}

func parseEvidenceRefs(value string) ([]improvement.EvidenceRef, error) {
	var refs []improvement.EvidenceRef
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		parts := strings.SplitN(item, ":", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
			return nil, fmt.Errorf("invalid evidence ref %q (want task_id:evidence_id)", item)
		}
		refs = append(refs, improvement.EvidenceRef{TaskID: strings.TrimSpace(parts[0]), EvidenceID: strings.TrimSpace(parts[1])})
	}
	return refs, nil
}

func renderEvidenceRefs(refs []improvement.EvidenceRef) string {
	parts := make([]string, 0, len(refs))
	for _, ref := range refs {
		parts = append(parts, ref.TaskID+":"+ref.EvidenceID)
	}
	return strings.Join(parts, ",")
}

func splitCSV(value string) []string {
	var parts []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			parts = append(parts, item)
		}
	}
	return parts
}

// writeFileAtomic writes data via temp file + rename so a partially written
// projection never looks like a complete artifact.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".runstead-improvement-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

func printImprovementHelp(out io.Writer) {
	fmt.Fprintln(out, "Usage: runstead improvement <subcommand> [flags]")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Issue #55 evidence-backed improvement proposals. Every command is an")
	fmt.Fprintln(out, "explicit OPERATOR action; there is no protocol tool for any of them.")
	fmt.Fprintln(out, "Proposals are non-authoritative: nothing in execution reads them, and")
	fmt.Fprintln(out, "applying one only produces a versioned declarative artifact a NEW task")
	fmt.Fprintln(out, "may use through the existing explicit --profile path.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Subcommands:")
	fmt.Fprintln(out, "  propose   create a pending proposal with durable provenance (source tasks, work units, evidence)")
	fmt.Fprintln(out, "  list      list proposals (--scope, --status filters)")
	fmt.Fprintln(out, "  show      inspect one proposal (--artifact prints the revision bytes)")
	fmt.Fprintln(out, "  review    operator decision: --decision approve|reject [--reason]")
	fmt.Fprintln(out, "  apply     materialize an approved proposal into a versioned artifact: --output PATH")
	fmt.Fprintln(out, "  validate  attach an objective validation record: --outcome and --evidence TASK:EVIDENCE")
	fmt.Fprintln(out, "  rollback  restore the previous revision deterministically [--reason]")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Common flags: --state-dir PATH")
	fmt.Fprintln(out, "Exit codes: 0 success, 1 not found, 2 usage, 3 state unavailable, 6 corrupt state")
}
