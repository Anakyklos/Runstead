package agent

import (
	"github.com/RenyEnnos/Runstead/internal/protocol"
	"github.com/RenyEnnos/Runstead/internal/tools"
)

// EvidenceSet holds successful observations produced during the current run.
// A final response is grounded only when every cited evidence ID belongs to
// this set: syntax alone never proves completion.
type EvidenceSet struct {
	observations map[string]tools.Observation
}

func NewEvidenceSet() *EvidenceSet {
	return &EvidenceSet{observations: make(map[string]tools.Observation)}
}

// Add records a successful observation as citable evidence. Failed or canceled
// observations carry no data and are never citable as evidence.
func (e *EvidenceSet) Add(observation tools.Observation) {
	if !observation.Success {
		return
	}
	if e.observations == nil {
		e.observations = make(map[string]tools.Observation)
	}
	e.observations[observation.ID] = observation
}

// Count returns the number of citable evidence IDs.
func (e *EvidenceSet) Count() int {
	return len(e.observations)
}

// Ground checks every cited ID against the collected observations. The
// evidence type binding is decided by the control-plane verifier against
// persisted evidence (issue #11 review); the run-level gate only proves the ID
// was produced by a successful observation in this run.
func (e *EvidenceSet) Ground(final protocol.FinalResponse) (grounded bool, missing []string) {
	if e.observations == nil {
		if len(final.Evidence) == 0 {
			return true, nil
		}
		missing = make([]string, 0, len(final.Evidence))
		for _, citation := range final.Evidence {
			missing = append(missing, citation.EvidenceID)
		}
		return false, missing
	}
	for _, citation := range final.Evidence {
		if _, ok := e.observations[citation.EvidenceID]; !ok {
			missing = append(missing, citation.EvidenceID)
		}
	}
	return len(missing) == 0, missing
}
