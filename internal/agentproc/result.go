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
// The cost accumulator is the session's, not the process's: Claude Code
// keeps a session's running total in the session and restores it on
// --resume, headless runs included, so a resumed process's total_cost_usd
// starts from what earlier processes on the session spent. The reader
// subtracts the total the launch was given (RunOptions.SessionCostBaselineUSD)
// before a Result reaches this fold, so CostUSD is the process's own spend and
// engagements add; SessionCostUSD keeps the raw total. duration_ms and
// num_turns are not restored, and each process reports its own.
//
// Disposition is sticky the other way: IsError and Interrupted, once seen,
// stay set, and the result text / subtype are the newest non-empty ones — a
// partial later outcome shouldn't blank a field that was already populated.
func MergeResult(base, resume *Result) *Result {
	merged := *base
	merged.CostUSD = resume.CostUSD
	merged.SessionCostUSD = resume.SessionCostUSD
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
	// Terminal and StopReason are the newest turn's, set or not: they
	// describe how that turn ended, and an earlier turn's ending is not this
	// one's.
	merged.Terminal = resume.Terminal
	merged.StopReason = resume.StopReason
	return &merged
}
