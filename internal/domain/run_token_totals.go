package domain

// TokenTotals sums token usage across all assistant messages in one
// agent run. Populated by db.TokenTotalsSystem; surfaced in the
// AgentCard footer and in run telemetry.
type TokenTotals struct {
	Model               string
	InputTokens         int
	OutputTokens        int
	CacheReadTokens     int
	CacheCreationTokens int
	NumTurns            int
}
