// Package policy implements the deterministic control-plane decision seam for
// write actions (issue #10).
//
// A policy decision is independent from the proposed write and from any model
// output: the static policy maps each write tool to a mode (allow, deny,
// approval_required) and, when approval is required, consults the persisted
// approvals table (control-plane state created by `runstead decide`). Model
// prose, reasoning, repository content and tool output can never grant or
// deny approval; they are not inputs to Evaluate.
//
// The seam is deliberately narrow so later milestones (for example #26
// subprocess execution) can extend it without coupling: a Policy implementation
// returns a typed Outcome for a typed Request, and the caller persists the
// decision.
package policy

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/RenyEnnos/Runstead/internal/tools"
)

// Decision is the typed policy outcome for one write proposal.
type Decision string

const (
	// Allowed means the write may execute.
	Allowed Decision = "allowed"
	// Denied means the write must not execute.
	Denied Decision = "denied"
	// ApprovalRequired means the write must not execute until the operator
	// control plane records an approval.
	ApprovalRequired Decision = "approval_required"
)

// Mode is the configured policy mode for one write tool.
type Mode string

const (
	// ModeAllow executes writes without a separate approval step.
	ModeAllow Mode = "allow"
	// ModeDeny never executes writes.
	ModeDeny Mode = "deny"
	// ModeApprovalRequired executes writes only after an operator approval.
	ModeApprovalRequired Mode = "approval_required"
)

// Request is the typed policy input for one write proposal. It is entirely
// control-plane and persisted state; it never contains model prose.
type Request struct {
	TaskID      string
	ActionID    string
	Tool        string
	Fingerprint string
	Workspace   string
}

// Outcome is the typed policy result. Reason is a stable typed reason, not
// free-form model text.
type Outcome struct {
	Decision Decision
	Reason   string
}

// Approval is the persisted operator decision for one task action.
type Approval struct {
	Decision string
	Reason   string
}

// Approvals is the narrow approval lookup a policy needs. Approvals are keyed
// by (task_id, fingerprint), the repeat/loop identity of the write proposal,
// so an approved write survives re-proposals. The store implements it; policy
// never touches SQL directly.
type Approvals interface {
	Approval(ctx context.Context, taskID, fingerprint string) (Approval, bool, error)
}

// ApprovalsFunc adapts a lookup function to the Approvals interface, so the
// composition root can wire the store without coupling policy to the
// persistence layer.
type ApprovalsFunc func(ctx context.Context, taskID, fingerprint string) (Approval, bool, error)

func (f ApprovalsFunc) Approval(ctx context.Context, taskID, fingerprint string) (Approval, bool, error) {
	return f(ctx, taskID, fingerprint)
}

// Policy is the control-plane decision seam. A nil-safe implementation must
// fail closed.
type Policy interface {
	Evaluate(ctx context.Context, request Request) Outcome
}

// Config maps write tools to their policy modes.
type Config struct {
	Modes map[string]Mode
}

// WriteTools returns the write tool names the policy can govern.
func WriteTools() []string {
	return []string{tools.ToolWriteFile, tools.ToolApplyPatch}
}

// Spec renders the canonical, sorted tool=mode specification of the config,
// for example "apply_patch=approval_required,write_file=allow". It is the
// durable serialization persisted with the task configuration and accepted by
// ParseConfig, so the effective write policy of a task round-trips across
// restart.
func (c Config) Spec() string {
	names := WriteTools()
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, tool := range names {
		mode, ok := c.Modes[tool]
		if !ok {
			continue
		}
		parts = append(parts, tool+"="+string(mode))
	}
	return strings.Join(parts, ",")
}

// Equal reports whether two configs assign the same mode to every registered
// write tool. A config that omits a tool is treated as the fail-closed
// default (approval_required), never as a permissive gap.
func Equal(a, b Config) bool {
	for _, tool := range WriteTools() {
		if modeOf(a, tool) != modeOf(b, tool) {
			return false
		}
	}
	return true
}

func modeOf(config Config, tool string) Mode {
	if mode, ok := config.Modes[tool]; ok {
		return mode
	}
	return ModeApprovalRequired
}

// DefaultConfig is the fail-closed default: every write tool requires
// operator approval before it can execute. Operators opt into allow or deny
// per tool through the CLI/configuration.
func DefaultConfig() Config {
	return Config{Modes: map[string]Mode{
		tools.ToolWriteFile:  ModeApprovalRequired,
		tools.ToolApplyPatch: ModeApprovalRequired,
	}}
}

// Static is the deterministic policy implementation: tool mode plus persisted
// operator approval lookup.
type Static struct {
	modes     map[string]Mode
	approvals Approvals
}

// NewStatic builds a Static policy. A nil approvals store fails closed: any
// approval-required tool is denied evaluation, never silently allowed.
func NewStatic(config Config, approvals Approvals) *Static {
	modes := make(map[string]Mode, len(config.Modes))
	for tool, mode := range config.Modes {
		modes[tool] = mode
	}
	return &Static{modes: modes, approvals: approvals}
}

// Evaluate returns the typed policy decision for one write proposal. The
// decision depends only on the configured mode and persisted approvals; the
// request carries no model content that could influence it.
func (p *Static) Evaluate(ctx context.Context, request Request) Outcome {
	mode, ok := p.modes[request.Tool]
	if !ok {
		// Fail closed: an ungoverned write tool never executes.
		return Outcome{Decision: Denied, Reason: "no_policy_for_tool"}
	}
	switch mode {
	case ModeAllow:
		return Outcome{Decision: Allowed, Reason: "policy_allow"}
	case ModeDeny:
		return Outcome{Decision: Denied, Reason: "policy_deny"}
	case ModeApprovalRequired:
		return p.evaluateApproval(ctx, request)
	default:
		return Outcome{Decision: Denied, Reason: "unknown_policy_mode"}
	}
}

func (p *Static) evaluateApproval(ctx context.Context, request Request) Outcome {
	if p.approvals == nil {
		return Outcome{Decision: ApprovalRequired, Reason: "approval_required"}
	}
	if request.Fingerprint == "" {
		// A proposal without a fingerprint cannot be matched to a durable
		// operator approval; fail closed.
		return Outcome{Decision: ApprovalRequired, Reason: "approval_required"}
	}
	approval, ok, err := p.approvals.Approval(ctx, request.TaskID, request.Fingerprint)
	if err != nil {
		// An approval lookup failure is not a denial by the operator, but it
		// is also not proof of approval: fail closed by requiring approval.
		return Outcome{Decision: ApprovalRequired, Reason: "approval_lookup_failed"}
	}
	if !ok {
		return Outcome{Decision: ApprovalRequired, Reason: "approval_required"}
	}
	switch approval.Decision {
	case "approved":
		return Outcome{Decision: Allowed, Reason: "approved_by_operator"}
	case "rejected":
		return Outcome{Decision: Denied, Reason: "rejected_by_operator"}
	default:
		return Outcome{Decision: ApprovalRequired, Reason: "approval_required"}
	}
}

// ParseConfig parses a --write-policy style value:
//
//	write_file=allow,apply_patch=approval_required
//
// Modes are allow, deny or approval_required; only registered write tools are
// accepted. Tools not mentioned keep the fail-closed default
// (approval_required), so a partial specification can never silently widen
// the policy. Unknown tools or modes are errors so a typo can never silently
// change the policy.
func ParseConfig(value string) (Config, error) {
	config := DefaultConfig()
	value = strings.TrimSpace(value)
	if value == "" {
		return Config{}, fmt.Errorf("write policy must not be empty")
	}
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		parts := strings.SplitN(item, "=", 2)
		if len(parts) != 2 {
			return Config{}, fmt.Errorf("invalid write policy %q: expected TOOL=MODE", item)
		}
		tool := strings.TrimSpace(parts[0])
		mode := strings.TrimSpace(parts[1])
		if tool != tools.ToolWriteFile && tool != tools.ToolApplyPatch {
			return Config{}, fmt.Errorf("invalid write policy tool %q: must be %s or %s", tool, tools.ToolWriteFile, tools.ToolApplyPatch)
		}
		switch Mode(mode) {
		case ModeAllow, ModeDeny, ModeApprovalRequired:
			config.Modes[tool] = Mode(mode)
		default:
			return Config{}, fmt.Errorf("invalid write policy mode %q: must be allow, deny or approval_required", mode)
		}
	}
	return config, nil
}
