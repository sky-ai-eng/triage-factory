package domain

// PromptStats holds aggregated performance data for a single prompt.
// Returned by db.PromptStore.Stats and rendered as JSON by
// /api/prompts/{id}/stats.
type PromptStats struct {
	TotalRuns     int        `json:"total_runs" doc:"How many agent conversations have run the prompt."`
	CompletedRuns int        `json:"completed_runs" doc:"How many of those conversations recorded their step's verdict, or belong to a blueprint run that completed."`
	FailedRuns    int        `json:"failed_runs" doc:"How many of those conversations failed because the infrastructure under the agent did."`
	SuccessRate   float64    `json:"success_rate" doc:"completed_runs as a share of total_runs, from 0 to 1, or 0 when the prompt has not run."` // 0-1
	AvgCostUSD    float64    `json:"avg_cost_usd" doc:"The average model spend of a conversation that ran the prompt, in USD."`
	AvgDurationMs int        `json:"avg_duration_ms" doc:"The average working time of a conversation that ran the prompt, in milliseconds."`
	TotalCostUSD  float64    `json:"total_cost_usd" doc:"The model spend of every conversation that ran the prompt, in USD."`
	LastUsedAt    *string    `json:"last_used_at" doc:"When the latest conversation on the prompt started, as an RFC 3339 timestamp, or null when none has."`           // RFC3339 or null — never-used surfaces as null in the API
	RunsPerDay    []DayCount `json:"runs_per_day" doc:"How many conversations started on the prompt on each of the last 30 days in UTC, today included, oldest first."` // last 30 days, oldest first
}

// DayCount is a single day's conversation count for the prompts-page sparkline.
type DayCount struct {
	Date  string `json:"date" doc:"The day, as YYYY-MM-DD."` // "2026-04-01"
	Count int    `json:"count" doc:"How many conversations started on the prompt that day."`
}
