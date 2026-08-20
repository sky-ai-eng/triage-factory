package agentproc

// MergeResult combines an initial Result with one from a resumed
// session so final accounting reflects total cost, duration, and turn
// count across all invocations. The result text comes from the resume
// (that's what callers want to report as the final outcome), but cost
// and turns are summed.
//
// If the resume's Result is empty, the base's value is preserved —
// a partial resume outcome shouldn't blank a field that was already
// populated.
func MergeResult(base, resume *Result) *Result {
	merged := *base
	merged.CostUSD += resume.CostUSD
	merged.DurationMs += resume.DurationMs
	merged.NumTurns += resume.NumTurns
	if resume.IsError {
		merged.IsError = true
	}
	if resume.Result != "" {
		merged.Result = resume.Result
	}
	if resume.Subtype != "" {
		merged.Subtype = resume.Subtype
	}
	if resume.Interrupted {
		merged.Interrupted = true
	}
	return &merged
}
