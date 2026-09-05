package protocol

const (
	actionEnvelopeShape = `<runstead_action>{"version":"runstead.protocol.v1","tool":"<registered-tool-name>","arguments":{...}}</runstead_action>`
	finalEnvelopeShape  = `<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"...","evidence":[{"evidence_id":"obs-...","tool":"<producer-tool>"}]}</runstead_final>`
)

// ActionEnvelopeGuidance is the canonical model-facing description of the
// action envelope. It deliberately describes a schema rather than inventing a
// tool invocation because the registered tool catalog is runtime-specific.
func ActionEnvelopeGuidance() string {
	return "Action envelope (exact shape):\n" +
		actionEnvelopeShape +
		"\nThe top-level JSON object contains exactly version, tool, and arguments.\n" +
		"version must be exactly runstead.protocol.v1.\n" +
		"tool must be one of the registered tools listed below.\n" +
		"arguments must be a JSON object matching that tool's declared arguments.\n" +
		"Use no Markdown code fence, no trailing comma, no pseudo-JSON, and no extra top-level fields.\n" +
		"Return exactly one envelope per model turn."
}

// FinalEnvelopeGuidance is the canonical model-facing description of the
// final envelope. Evidence is grounded in observations and completion remains
// subject to independent Runstead verification.
func FinalEnvelopeGuidance() string {
	return "Final envelope (exact shape):\n" +
		finalEnvelopeShape +
		"\nThe top-level JSON object contains exactly version, status, summary, and evidence.\n" +
		"version must be exactly runstead.protocol.v1.\n" +
		"status must be exactly complete or incomplete.\n" +
		"evidence uses exactly evidence_id and tool for each entry and must be a non-empty array. Evidence IDs must have been observed, and the cited tool must be the producer tool.\n" +
		"A task cannot invent evidence merely to produce a final response. Completion remains a proposal and is independently verified by Runstead."
}

func correctionGuidance(code FailureCode) string {
	const common = "Return exactly one valid runstead_action or runstead_final envelope; never claim local execution without an envelope."

	switch code {
	case FailureMalformedJSON:
		return common + " Malformed JSON: return one strict JSON object inside one matching envelope. Use no Markdown code fence, no trailing comma, and no pseudo-JSON. Use either " + actionEnvelopeShape + " or " + finalEnvelopeShape + "."
	case FailureInvalidActionSchema, FailureInvalidArguments, FailureUnknownTool:
		return common + " Invalid action schema: use exactly the fields version, tool, and arguments, with version and tool as strings and arguments as a JSON object matching a registered tool's declared arguments. No extra fields. Shape: " + actionEnvelopeShape + "."
	case FailureInvalidFinalSchema:
		return common + " Invalid final schema: use exactly the fields version, status, summary, and evidence. Status is complete or incomplete; evidence is a non-empty array of entries with exactly evidence_id and tool. Shape: " + finalEnvelopeShape + ". Cite only observed evidence."
	case FailureMissingEnvelope, FailureUnclosedEnvelope:
		return common + " Return exactly one envelope with matching opening and closing tags: one <runstead_action>...</runstead_action> or one <runstead_final>...</runstead_final>; do not return prose instead of the envelope."
	case FailureMultipleEnvelopes:
		return common + " Do not return multiple envelopes. Return exactly one envelope with one matching opening and closing tag pair for this model turn."
	case FailureUnsupportedProtocolVersion:
		return common + " The version must be exactly runstead.protocol.v1, not another version. Return one strict action or final envelope."
	case FailureProtocolRefusal, FailureUnsupportedExecutionClaim:
		return common + " Do not describe an execution claim or refusal as a substitute for the required envelope."
	case FailureRepeatedAction:
		return common + " Do not repeat the same action proposal. Choose a different valid registered action or return a grounded final envelope using observed evidence."
	default:
		return common
	}
}
