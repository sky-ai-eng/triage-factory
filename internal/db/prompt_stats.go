package db

import (
	"database/sql"
	"time"
)

// PromptStats holds aggregated performance data for a single prompt.
type PromptStats struct {
	TotalRuns     int        `json:"total_runs"`
	CompletedRuns int        `json:"completed_runs"`
	FailedRuns    int        `json:"failed_runs"`
	SuccessRate   float64    `json:"success_rate"` // 0-1
	AvgCostUSD    float64    `json:"avg_cost_usd"`
	AvgDurationMs int        `json:"avg_duration_ms"`
	TotalCostUSD  float64    `json:"total_cost_usd"`
	LastUsedAt    *string    `json:"last_used_at"` // RFC3339 or null
	RunsPerDay    []DayCount `json:"runs_per_day"` // last 30 days
}

// DayCount is a single day's run count for the sparkline.
type DayCount struct {
	Date  string `json:"date"` // "2026-04-01"
	Count int    `json:"count"`
}

// GetPromptStats returns aggregated stats for a prompt from agent_runs.
func GetPromptStats(db *sql.DB, promptID string) (*PromptStats, error) {
	stats := &PromptStats{}

	// Totals
	err := db.QueryRow(`
		SELECT
			COUNT(*),
			SUM(CASE WHEN status = 'completed' THEN 1 ELSE 0 END),
			SUM(CASE WHEN status = 'failed' THEN 1 ELSE 0 END),
			COALESCE(AVG(total_cost_usd), 0),
			COALESCE(AVG(duration_ms), 0),
			COALESCE(SUM(total_cost_usd), 0)
		FROM agent_runs WHERE prompt_id = ?
	`, promptID).Scan(
		&stats.TotalRuns,
		&stats.CompletedRuns,
		&stats.FailedRuns,
		&stats.AvgCostUSD,
		&stats.AvgDurationMs,
		&stats.TotalCostUSD,
	)
	if err != nil {
		return nil, err
	}

	if stats.TotalRuns > 0 {
		stats.SuccessRate = float64(stats.CompletedRuns) / float64(stats.TotalRuns)
	}

	// Last used — sql.ErrNoRows is expected for prompts that have never run.
	// Any other error leaves lastUsed invalid, which we handle identically.
	var lastUsed sql.NullTime
	_ = db.QueryRow(`SELECT MAX(started_at) FROM agent_runs WHERE prompt_id = ?`, promptID).Scan(&lastUsed)
	if lastUsed.Valid {
		s := lastUsed.Time.Format(time.RFC3339)
		stats.LastUsedAt = &s
	}

	// Runs per day (last 30 days)
	cutoff := time.Now().AddDate(0, 0, -30).Format("2006-01-02")
	rows, err := db.Query(`
		SELECT DATE(started_at) as day, COUNT(*) as cnt
		FROM agent_runs
		WHERE prompt_id = ? AND DATE(started_at) >= ?
		GROUP BY day ORDER BY day
	`, promptID, cutoff)
	if err != nil {
		return stats, nil // non-fatal
	}
	defer rows.Close()

	dayMap := make(map[string]int)
	for rows.Next() {
		var day string
		var cnt int
		if err := rows.Scan(&day, &cnt); err != nil {
			continue
		}
		dayMap[day] = cnt
	}

	// Fill in all 30 days
	for i := 29; i >= 0; i-- {
		d := time.Now().AddDate(0, 0, -i).Format("2006-01-02")
		stats.RunsPerDay = append(stats.RunsPerDay, DayCount{Date: d, Count: dayMap[d]})
	}

	return stats, nil
}
