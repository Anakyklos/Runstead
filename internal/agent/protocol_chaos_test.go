package agent_test

// Issue #13 - model/protocol chaos matrix at runtime level. Each case drives
// the REAL bounded loop with the REAL durable store through a malformed or
// hostile model response and asserts the typed outcome plus the authoritative
// persisted state afterwards. The protocol parser itself is unit-tested in
// internal/protocol and the shared corpus in corpus_test.go; this matrix adds
// the runtime evidence those tests cannot: typed loop outcomes, bounded
// corrections, durable action/attempt projections and proof that no chaos
// case creates an unauthorized effect.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/agent"
	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/state"
)

// protocolChaosCase is one row of the #13 model/protocol failure matrix.
type protocolChaosCase struct {
	name string
	// responses is the scripted provider stream for the run.
	responses []string
	// limits overrides the loop limits (zero fields keep defaults).
	limits agent.Limits
	// wantOutcome is the expected typed terminal outcome.
	wantOutcome agent.Outcome
	// wantReason is a substring the stop reason must contain (optional).
	wantReason string
	// wantActions is the expected number of persisted logical actions
	// (accepted envelopes) after the run.
	wantActions int
	// wantToolAttempts is the expected number of persisted tool attempts.
	wantToolAttempts int
	// wantTerminal asserts the task row is terminal (finalized), not
	// resumable. Terminal is the default; a chaos case may override it to
	// prove a control-plane pause instead.
	wantTerminal bool
	// wantCompleted asserts the task must never be completed.
	wantCompleted bool
}

func TestProtocolChaosMatrix(t *testing.T) {
	cases := []protocolChaosCase{
		{
			name:        "empty model response",
			responses:   []string{""},
			wantOutcome: agent.OutcomeProviderFailure,
			wantReason:  "empty_response",
			wantActions: 0,
			// The empty response consumed exactly one governed attempt and no
			// tool attempt was created.
			wantToolAttempts: 0,
			wantTerminal:     true,
			wantCompleted:    true,
		},
		{
			name: "truncated response envelope",
			// The truncated envelope is a reasonable correction; with a
			// zero-value MaxCorrections the default of 2 applies, so the loop
			// corrects twice and exhausts without ever executing anything.
			responses: []string{
				`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}`,
				`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}`,
				`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}`,
			},
			limits:           agent.Limits{MaxCorrections: 2},
			wantOutcome:      agent.OutcomeCorrectionsExhausted,
			wantActions:      0,
			wantToolAttempts: 0,
			wantTerminal:     true,
			wantCompleted:    true,
		},
		{
			name: "malformed action JSON",
			responses: []string{
				`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":}}`,
				`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":}}`,
				`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":}}`,
			},
			limits:           agent.Limits{MaxCorrections: 2},
			wantOutcome:      agent.OutcomeCorrectionsExhausted,
			wantActions:      0,
			wantToolAttempts: 0,
			wantTerminal:     true,
			wantCompleted:    true,
		},
		{
			name: "multiple conflicting action envelopes",
			// Two action envelopes in one response are rejected as a single
			// parse failure (multiple_envelopes); the model then proposes ONE
			// valid action that executes, so neither conflicting envelope
			// creates an effect and the corrected proposal is the only effect.
			responses: []string{
				`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>` +
					`<runstead_action>{"version":"runstead.protocol.v1","tool":"write_file","arguments":{"path":"a.txt","content":"evil\n"}}</runstead_action>`,
				`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
				`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
			},
			limits:           agent.Limits{MaxCorrections: 2, MaxRepeatedActions: 2},
			wantOutcome:      agent.OutcomeCompleted,
			wantActions:      1,
			wantToolAttempts: 1,
			wantTerminal:     true,
			wantCompleted:    false,
		},
		{
			name: "unknown tool",
			// The unknown tool is corrected (never executed); the next valid
			// proposal completes normally.
			responses: []string{
				`<runstead_action>{"version":"runstead.protocol.v1","tool":"delete_everything","arguments":{"path":"/"}}</runstead_action>`,
				`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
				`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
			},
			limits:           agent.Limits{MaxCorrections: 2, MaxRepeatedActions: 2},
			wantOutcome:      agent.OutcomeCompleted,
			wantActions:      1,
			wantToolAttempts: 1,
			wantTerminal:     true,
			wantCompleted:    false,
		},
		{
			name: "identical repeated action",
			// The second identical proposal is rejected by the repeat guard
			// with a durable 'rejected' projection; the third exceeds the
			// allowance of one repeat and stops typed.
			responses: []string{
				`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
				`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
				`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
			},
			limits:           agent.Limits{MaxRepeatedActions: 1},
			wantOutcome:      agent.OutcomeRepeatedAction,
			wantActions:      3,
			wantToolAttempts: 1,
			wantTerminal:     true,
			wantCompleted:    true,
		},
		{
			name: "completion without evidence",
			// The final cites an evidence id no observation produced: the
			// grounding gate rejects it and the task is finalized as
			// final_not_grounded, never completed.
			responses: []string{
				`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
				`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"I did it","evidence":[{"evidence_id":"obs-000999","tool":"read_file"}]}</runstead_final>`,
			},
			limits:           agent.Limits{MaxSteps: 10},
			wantOutcome:      agent.OutcomeFinalNotGrounded,
			wantActions:      1,
			wantToolAttempts: 1,
			wantTerminal:     true,
			wantCompleted:    true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			writeFixture(t, workspace, "a.txt", "alpha\n")

			responses := make([]providerResponse, 0, len(tc.responses))
			for _, text := range tc.responses {
				responses = append(responses, providerResponse{text: text})
			}
			h := newWriteHarness(t, workspace, allowAllPolicy(), nil, rawResponses(responses...)...)
			// The acceptance plan lets the well-behaved rows reach completed;
			// failing rows never reach the verifier, so the plan is inert there.
			loop := h.loopWithPlan(t, tc.limits, existsPlan("a.txt"), nil)

			result := loop.Run(context.Background(), testTask("task-protocol-chaos"))
			if result.Outcome != tc.wantOutcome {
				t.Fatalf("outcome = %q, want %q (reason %q)", result.Outcome, tc.wantOutcome, result.StopReason)
			}
			if tc.wantReason != "" && !strings.Contains(result.StopReason, tc.wantReason) {
				t.Fatalf("stop reason %q must contain %q", result.StopReason, tc.wantReason)
			}

			// Authoritative final state: the task row, actions and tool
			// attempts must reflect exactly what happened, nothing more.
			snapshot, err := h.store.LoadRecoverySnapshot(context.Background(), "task-protocol-chaos")
			if err != nil {
				t.Fatalf("LoadRecoverySnapshot() error = %v", err)
			}
			if len(snapshot.Actions) != tc.wantActions {
				t.Fatalf("persisted actions = %d, want %d", len(snapshot.Actions), tc.wantActions)
			}
			if len(snapshot.ToolAttempts) != tc.wantToolAttempts {
				t.Fatalf("persisted tool attempts = %d, want %d", len(snapshot.ToolAttempts), tc.wantToolAttempts)
			}
			// The status projection collapses terminal failures to 'failed';
			// the typed outcome is asserted through result.Outcome above and
			// the persisted outcome column below.
			wantStatus := "failed"
			if tc.wantOutcome == agent.OutcomeCompleted {
				wantStatus = "completed"
			}
			if !tc.wantTerminal {
				wantStatus = "running"
			}
			if snapshot.Task.Status != wantStatus {
				t.Fatalf("task status = %q, want %q", snapshot.Task.Status, wantStatus)
			}
			rendered := renderedInspect(t, h.store, "task-protocol-chaos")
			if !strings.Contains(rendered, "Outcome: "+string(tc.wantOutcome)) {
				t.Fatalf("inspect must render the typed outcome %q:\n%s", tc.wantOutcome, rendered)
			}
			if tc.wantCompleted && strings.Contains(rendered, "Outcome: completed") {
				t.Fatal("task must never be persisted as completed under this chaos case")
			}
			// No chaos case may create effects outside the tool attempts it
			// explicitly accounts for.
			content, err := readFileContent(workspace, "a.txt")
			if err != nil {
				t.Fatalf("workspace file unreadable: %v", err)
			}
			if content != "alpha\n" {
				t.Fatalf("workspace content = %q, want unchanged alpha\\n", content)
			}
		})
	}
}

// TestProtocolChaosRepeatedActionRejectedProjection is the durable-state
// detail of the repeated-action row: the guard-rejected proposal must be
// persisted as 'rejected', never 'planned' or 'completed', because the loop
// may stop right after the rejection (issue #13 final-state invariant).
func TestProtocolChaosRepeatedActionRejectedProjection(t *testing.T) {
	workspace := t.TempDir()
	writeFixture(t, workspace, "a.txt", "alpha\n")

	h := newWriteHarness(t, workspace, allowAllPolicy(), nil,
		actionResponse("read_file", `{"path":"a.txt"}`),
		actionResponse("read_file", `{"path":"a.txt"}`),
		actionResponse("read_file", `{"path":"a.txt"}`),
	)
	loop := h.loop(t, agent.Limits{MaxRepeatedActions: 1})
	result := loop.Run(context.Background(), testTask("task-repeat-projection"))
	if result.Outcome != agent.OutcomeRepeatedAction {
		t.Fatalf("outcome = %q, want repeated_action", result.Outcome)
	}
	snapshot, err := h.store.LoadRecoverySnapshot(context.Background(), "task-repeat-projection")
	if err != nil {
		t.Fatal(err)
	}
	statuses := make([]string, 0, len(snapshot.Actions))
	for _, action := range snapshot.Actions {
		statuses = append(statuses, action.Status)
	}
	// First action executed (completed projection happens through its tool
	// attempt), second and third rejected by the guard.
	if len(snapshot.Actions) != 3 {
		t.Fatalf("actions = %d, want 3 (%v)", len(snapshot.Actions), statuses)
	}
	if snapshot.Actions[1].Status != "rejected" || snapshot.Actions[2].Status != "rejected" {
		t.Fatalf("guard-rejected actions = %v, want rejected/rejected", statuses)
	}
}

// rawResponses converts raw response text into provider.Response values.
type providerResponse struct {
	text string
}

func rawResponses(values ...providerResponse) []provider.Response {
	responses := make([]provider.Response, 0, len(values))
	for _, value := range values {
		responses = append(responses, provider.Response{Text: value.text})
	}
	return responses
}

func readFileContent(root, name string) (string, error) {
	content, err := os.ReadFile(filepath.Join(root, name))
	return string(content), err
}

// renderedInspect renders one task through the durable store's authoritative
// inspect renderer (the same surface `runstead inspect` uses).
func renderedInspect(t *testing.T, store *state.Store, taskID string) string {
	t.Helper()
	var builder strings.Builder
	if err := store.RenderInspect(context.Background(), &builder, taskID); err != nil {
		t.Fatalf("RenderInspect(%s) error = %v", taskID, err)
	}
	return builder.String()
}
