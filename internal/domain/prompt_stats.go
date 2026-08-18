package domain

// PromptStats holds aggregated performance data for a single prompt.
// Returned by db.PromptStore.Stats and rendered as JSON by
// /api/prompts/{id}/stats.
type PromptStats struct {
	TotalRuns     int        `json:"total_runs"`
	CompletedRuns int        `json:"completed_runs"`
	FailedRuns    int        `json:"failed_runs"`
	SuccessRate   float64    `json:"success_rate"` // 0-1
	AvgCostUSD    float64    `json:"avg_cost_usd"`
	AvgDurationMs int        `json:"avg_duration_ms"`
	TotalCostUSD  float64    `json:"total_cost_usd"`
	LastUsedAt    *string    `json:"last_used_at"` // RFC3339 or null — never-used surfaces as null in the API
	RunsPerDay    []DayCount `json:"runs_per_day"` // last 30 days, oldest first
}

// DayCount is a single day's conversation count for the prompts-page sparkline.
type DayCount struct {
	Date  string `json:"date"` // "2026-04-01"
	Count int    `json:"count"`
}
