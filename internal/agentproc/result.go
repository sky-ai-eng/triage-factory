package agentproc

// MergeResult folds one process's successive per-turn Results into a single
// Result for the whole invocation.
//
// The accounting fields (CostUSD, DurationMs, NumTurns) are RUNNING TOTALS
// on the wire: the SDK reports every result's total_cost_usd from one
// process-wide accumulator, its duration_ms from one clock started at the
// query, and its num_turns as the turns so far — so the newest result already
// contains every earlier one, and the fold takes it as-is rather than adding.
// Summing would bill a three-turn process a + (a+b) + (a+b+c).
//
// A fresh process starts that accumulator at zero, including one resumed by
// session id: the SDK restores a prior total only for a session the
// interactive REPL recorded as its last, which a headless run never is. So a
// process's final total is that engagement's own spend, and engagements add.
//
// Disposition is sticky the other way: IsError and Interrupted, once seen,
// stay set, and the result text / subtype are the newest non-empty ones — a
// partial later outcome shouldn't blank a field that was already populated.
func MergeResult(base, resume *Result) *Result {
	merged := *base
	merged.CostUSD = resume.CostUSD
	merged.DurationMs = resume.DurationMs
	merged.NumTurns = resume.NumTurns
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
