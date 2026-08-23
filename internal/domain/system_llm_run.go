package domain

import "time"

// SystemLLMRun is one row in system_llm_runs — the per-call accounting
// for a headless LLM invocation made by a background system job (the
// scorer or repo-profiler). One row per agentproc.Run call.
//
// Org-level, no team_id by design — scorer batches mix teams, and
// repositories/entities carry no team. The token breakdown (input /
// output / cache_read / cache_creation) is captured for cache-rate
// analysis. CostUSD / DurationMs / NumTurns / IsError come from the
// terminal result event the subprocess emits; the token counts are
// summed across the run's assistant messages by agentproc.UsageSink.
// See TFAC-451.
type SystemLLMRun struct {
	ID    string `json:"id"`
	OrgID string `json:"org_id"`
	// Job is the discriminator: "scorer" | "repo_profiler".
	// Free text (extensible — no CHECK constraint).
	Job   string `json:"job"`
	Model string `json:"model"`

	TotalCostUSD        float64 `json:"total_cost_usd"`
	InputTokens         int     `json:"input_tokens"`
	OutputTokens        int     `json:"output_tokens"`
	CacheReadTokens     int     `json:"cache_read_tokens"`
	CacheCreationTokens int     `json:"cache_creation_tokens"`

	DurationMs int  `json:"duration_ms"`
	NumTurns   int  `json:"num_turns"`
	IsError    bool `json:"is_error"`

	// MetadataJSON is optional per-job context, e.g. {"batch_size": 10}
	// (scorer), {"repo_count": 3} (repo_profiler). Empty string serializes
	// to SQL NULL.
	MetadataJSON string `json:"metadata_json,omitempty"`

	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`

	// TraceID is the agentproc invocation's Claude Code session id — the
	// idempotency key (TFAC-579). It is NOT the caller-supplied agentproc
	// TraceID (that's a per-job-type label like "scorer-batch", reused
	// across every invocation of that job, so it can't identify one call).
	// Empty when the subprocess never got far enough to mint a session;
	// the store's insert only enforces uniqueness when non-empty.
	TraceID string `json:"trace_id,omitempty"`
}
