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
// evidence type distinction is reserved for later milestones; today an ID is
// compatible when it was produced by a successful observation in this run.
func (e *EvidenceSet) Ground(final protocol.FinalResponse) (grounded bool, missing []string) {
	if e.observations == nil {
		return len(final.Evidence) == 0, append([]string(nil), final.Evidence...)
	}
	for _, id := range final.Evidence {
		if _, ok := e.observations[id]; !ok {
			missing = append(missing, id)
		}
	}
	return len(missing) == 0, missing
}
