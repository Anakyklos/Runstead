package context

import (
	"errors"
	"fmt"
	"sort"

	"github.com/RenyEnnos/Runstead/internal/state"
)

// Compiler is a deterministic, bounded, evidence-preserving projector of
// authoritative task state into model context (issue #51). It is a pure
// function of its Input: equal input + equal budget + equal version produce
// byte-identical output. It never touches the provider, the transcript or the
// workspace.
type Compiler struct{}

// Compile projects the authoritative input into a Compiled context. It fails
// with ErrBudgetExhausted (wrapped) when the mandatory/pinned content cannot
// fit inside the budget; no truncated projection is returned in that case.
func (c *Compiler) Compile(input Input) (Compiled, error) {
	if input.Snapshot == nil {
		return Compiled{}, errors.New("context compiler: snapshot is required")
	}
	budget := input.Budget
	if budget.MaxContextBytes == 0 {
		budget = DefaultBudget()
	}
	if err := budget.Validate(); err != nil {
		return Compiled{}, err
	}
	// Deterministic extraction: every helper sorts by an explicit key; no
	// map iteration order can reach the output path.
	model := extract(input, budget)
	diagnostics := Diagnostics{
		CompilerVersion: CompilerVersion,
		Budget:          budget,
		Counts:          make(map[FactKind]int, len(sortedFactKinds)),
	}
	for _, fact := range model.facts {
		diagnostics.Counts[fact.Kind]++
	}

	render, byteOmitted, err := renderCompiled(model, budget)
	if err != nil {
		// Fail closed: mandatory content did not fit. Diagnostics carry the
		// reason but no partial projection is produced.
		diagnostics.ExhaustionReason = err.Error()
		return Compiled{
			Authoritative:    model.facts,
			NonAuthoritative: model.notes,
			Diagnostics:      diagnostics,
		}, fmt.Errorf("%w: %s", ErrBudgetExhausted, err)
	}
	diagnostics.Omitted = append(append([]OmittedItem(nil), model.omitted...), byteOmitted...)

	ids := make([]string, 0, len(model.evidenceIDs))
	ids = append(ids, model.evidenceIDs...)
	sort.Strings(ids) // deterministic regardless of extraction order
	render = state.Redact(render)
	return Compiled{
		Authoritative:    model.facts,
		NonAuthoritative: model.notes,
		Diagnostics:      diagnostics,
		render:           render,
		evidenceIDs:      ids,
	}, nil
}

// model is the extracted intermediate: facts in deterministic order, notes,
// pinned evidence IDs and the per-kind selections used by the renderer.
type model struct {
	facts       []Fact
	notes       []Note
	evidenceIDs []string
	// sections holds the breakdown of mandatory vs degradable lines by
	// section, already ordered; renderCompiled accounts bytes over it.
	sections []section
	// omitted records cap-based omissions decided during extraction (detail
	// lines dropped because their section cap was exhausted). Byte-budget
	// omissions are appended by renderCompiled.
	omitted []OmittedItem
}

// section is one fixed-order rendering section. Pinned lines must always fit;
// degradable lines are selected deterministically until the budget runs out
// and each skipped line is recorded in Omitted.
type section struct {
	kind       FactKind
	pinned     []string
	degradable []degradableLine
}

type degradableLine struct {
	text string
	id   string // omitted-id recorded when skipped ("" = not recorded)
}

// renderCompiled renders the fixed-order sections under the byte budget.
// Returns the rendered text (without trailing newline) and the omitted
// records. When the pinned content alone exceeds the budget it returns an
// error and no text.
func renderCompiled(model model, budget Budget) (string, []OmittedItem, error) {
	var required int
	for _, sec := range model.sections {
		for _, line := range sec.pinned {
			required += len(line) + 1
		}
	}
	// The pinned sections always include the authority preamble and the
	// explicit non-authoritative marker, which the extractor places in
	// pinned lines; an empty model still has a required minimum.
	if required > budget.MaxContextBytes {
		return "", nil, fmt.Errorf("mandatory content requires %d bytes, budget is %d", required, budget.MaxContextBytes)
	}

	var omitted []OmittedItem
	builder := newDeterministicBuilder(budget.MaxContextBytes - required)
	for _, sec := range model.sections {
		for _, line := range sec.pinned {
			builder.write(line)
		}
		for index, line := range sec.degradable {
			if !builder.tryWrite(line.text) {
				// The failing line is recorded once, and only the lines
				// genuinely after it (never selected) are recorded, each
				// exactly once: rendered lines and duplicates never appear
				// in Diagnostics.Omitted (issue #51 review).
				if line.id != "" {
					omitted = append(omitted, OmittedItem{Kind: sec.kind, ID: line.id})
				}
				for _, rest := range sec.degradable[index+1:] {
					if rest.id != "" {
						omitted = append(omitted, OmittedItem{Kind: sec.kind, ID: rest.id})
					}
				}
				break
			}
		}
	}
	return builder.String(), omitted, nil
}

// deterministicBuilder accumulates newline-terminated lines with a hard
// remaining budget. tryWrite returns false (without effect) when the line
// does not fit.
type deterministicBuilder struct {
	buf       []byte
	remaining int
}

func newDeterministicBuilder(remaining int) *deterministicBuilder {
	return &deterministicBuilder{remaining: remaining}
}

func (b *deterministicBuilder) write(line string) {
	// Pinned lines were accounted in the mandatory byte sum before the
	// builder existed: they fit by construction and are appended
	// unconditionally. A pinned line can never be dropped silently.
	b.buf = append(b.buf, line...)
	b.buf = append(b.buf, '\n')
	b.remaining -= len(line) + 1
}

func (b *deterministicBuilder) tryWrite(line string) bool {
	if len(line)+1 > b.remaining {
		return false
	}
	b.buf = append(b.buf, line...)
	b.buf = append(b.buf, '\n')
	b.remaining -= len(line) + 1
	return true
}

func (b *deterministicBuilder) String() string {
	return string(b.buf)
}
