package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/RenyEnnos/Runstead/internal/tools"
)

const (
	roleSystem       = "system"
	roleUser         = "user"
	roleAssistant    = "assistant"
	roleObservation  = "observation"
	roleCorrection   = "correction"
	roleRecovery     = "recovery"
	roleVerification = "verification"
)

type message struct {
	role    string
	content string
}

// transcript is the deterministic model-facing context. The system contract
// and task prompt occupy the first two sections; model responses, untrusted
// observations and corrections are appended per turn. Observations always use
// the observation role so repository content stays structurally separate from
// the system contract and control plane.
type transcript struct {
	messages []message
}

func newTranscript(contract, task string) *transcript {
	t := &transcript{messages: make([]message, 0, 8)}
	t.append(roleSystem, contract)
	if strings.TrimSpace(task) != "" {
		t.append(roleUser, task)
	}
	return t
}

func (t *transcript) append(role, content string) {
	t.messages = append(t.messages, message{role: role, content: content})
}

func (t *transcript) assistant(content string) {
	t.append(roleAssistant, content)
}

func (t *transcript) correction(content string) {
	t.append(roleCorrection, content)
}

// recovery appends the bounded reconstruction summary of an interrupted task
// under the recovery role. It sits between the task prompt and the first new
// model turn, so the model continues from durable state instead of the old
// provider conversation.
func (t *transcript) recovery(content string) {
	if strings.TrimSpace(content) != "" {
		t.append(roleRecovery, content)
	}
}

// observation marshals the tool observation and appends it under the
// observation role. The tools package already sets Metadata.Untrusted; the
// role marker and the serialized marker together keep the boundary explicit.
func (t *transcript) observation(observation tools.Observation) {
	encoded, err := json.Marshal(observation)
	if err != nil {
		encoded = []byte(`{"success":false,"failure":{"code":"marshal_failure","message":"observation could not be serialized"}}`)
	}
	t.append(roleObservation, string(encoded))
}

// verification appends a bounded, structured control-plane verification
// result under the verification role (issue #11). It is NOT a protocol
// correction: the model produced a valid final; the environment did not
// satisfy completion. The result is bounded to the verifier report limits and
// contains only typed check results, never raw file contents or model text.
func (t *transcript) verification(decision, summary string, checks []verificationCheckView) {
	encoded, err := json.Marshal(struct {
		Type     string                  `json:"type"`
		Decision string                  `json:"decision"`
		Summary  string                  `json:"summary"`
		Checks   []verificationCheckView `json:"checks"`
	}{Type: "verification", Decision: decision, Summary: summary, Checks: checks})
	if err != nil {
		encoded = []byte(`{"type":"verification","decision":"blocked","summary":"verification result could not be serialized"}`)
	}
	t.append(roleVerification, string(encoded))
}

// verificationCheckView is the bounded per-check view sent to the model. It
// is structurally separated from tool observations so model text can never
// influence the verifier: this is a one-way report of control-plane results.
type verificationCheckView struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"`
	Status   string   `json:"status"`
	Expected string   `json:"expected,omitempty"`
	Observed string   `json:"observed,omitempty"`
	Evidence []string `json:"evidence_ids,omitempty"`
	Reason   string   `json:"reason,omitempty"`
}

func (t *transcript) render() string {
	var builder strings.Builder
	for _, entry := range t.messages {
		fmt.Fprintf(&builder, "=== runstead:%s ===\n%s\n", entry.role, entry.content)
	}
	return builder.String()
}
