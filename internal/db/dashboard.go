package db

import (
	"database/sql"
	"encoding/json"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// DashboardStats holds aggregated PR statistics derived from entity snapshots.
type DashboardStats struct {
	Merged          int              `json:"merged"`
	Closed          int              `json:"closed"`
	Awaiting        int              `json:"awaiting"`
	Draft           int              `json:"draft"`
	ReviewsGiven    int              `json:"reviews_given"`
	ReviewsReceived int              `json:"reviews_received"`
	MergedOverTime  []DashboardPoint `json:"merged_over_time"`
}

type DashboardPoint struct {
	Week  string `json:"week"`
	Count int    `json:"count"`
}

// PRSummaryRow is a PR as displayed on the dashboard list.
type PRSummaryRow struct {
	Number    int      `json:"number"`
	Title     string   `json:"title"`
	Repo      string   `json:"repo"`
	Author    string   `json:"author"`
	State     string   `json:"state"`
	Draft     bool     `json:"draft"`
	Labels    []string `json:"labels"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
	HTMLURL   string   `json:"html_url"`
}

// GetDashboardStats computes dashboard statistics from entity snapshots.
// username is the authenticated user's GitHub login, used to attribute reviews.
func GetDashboardStats(database *sql.DB, username string, sinceDays int) (*DashboardStats, error) {
	since := time.Now().AddDate(0, 0, -sinceDays)

	rows, err := database.Query(`
		SELECT snapshot_json FROM entities
		WHERE source = 'github' AND snapshot_json IS NOT NULL AND snapshot_json != ''
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	stats := &DashboardStats{}
	mergedByWeek := make(map[string]int)

	for rows.Next() {
		var snapJSON string
		if err := rows.Scan(&snapJSON); err != nil {
			continue
		}

		var snap domain.PRSnapshot
		if err := json.Unmarshal([]byte(snapJSON), &snap); err != nil {
			continue
		}

		if snap.Author == username {
			// Our PR — count status and reviews received
			switch {
			case snap.Merged:
				mergedAt, err := time.Parse(time.RFC3339, snap.MergedAt)
				if err == nil && mergedAt.After(since) {
					stats.Merged++
					mergedByWeek[mondayOf(mergedAt)]++
				}

			case snap.State == "CLOSED":
				closedAt, err := time.Parse(time.RFC3339, snap.ClosedAt)
				if err == nil && closedAt.After(since) {
					stats.Closed++
				}

			case snap.State == "OPEN" && snap.IsDraft:
				stats.Draft++

			case snap.State == "OPEN":
				stats.Awaiting++
			}

			for _, review := range snap.Reviews {
				if review.Author != username {
					stats.ReviewsReceived++
				}
			}
		} else {
			// Someone else's PR — only count reviews we gave
			for _, review := range snap.Reviews {
				if review.Author == username {
					stats.ReviewsGiven++
				}
			}
		}
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Build merged timeline
	stats.MergedOverTime = buildTimeline(mergedByWeek, 5)

	return stats, nil
}

// GetDashboardPRs returns PR summaries from entities for the dashboard list.
// Only includes PRs authored by the given username.
func GetDashboardPRs(database *sql.DB, username string) ([]PRSummaryRow, error) {
	rows, err := database.Query(`
		SELECT snapshot_json FROM entities
		WHERE source = 'github' AND snapshot_json IS NOT NULL AND snapshot_json != ''
		ORDER BY last_polled_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var prs []PRSummaryRow
	for rows.Next() {
		var snapJSON string
		if err := rows.Scan(&snapJSON); err != nil {
			continue
		}

		var snap domain.PRSnapshot
		if err := json.Unmarshal([]byte(snapJSON), &snap); err != nil {
			continue
		}
		if snap.Author != username {
			continue
		}

		state := stateToLower(snap.State)
		if snap.Merged {
			state = "merged"
		}

		prs = append(prs, PRSummaryRow{
			Number:    snap.Number,
			Title:     snap.Title,
			Repo:      snap.Repo,
			Author:    snap.Author,
			State:     state,
			Draft:     snap.IsDraft,
			Labels:    snap.Labels,
			CreatedAt: snap.CreatedAt,
			UpdatedAt: snap.UpdatedAt,
			HTMLURL:   snap.URL,
		})
	}

	return prs, rows.Err()
}

func stateToLower(s string) string {
	switch s {
	case "OPEN":
		return "open"
	case "CLOSED":
		return "closed"
	case "MERGED":
		return "merged"
	default:
		return s
	}
}

func mondayOf(t time.Time) string {
	weekday := int(t.Weekday())
	if weekday == 0 {
		weekday = 7
	}
	monday := t.AddDate(0, 0, -(weekday - 1))
	return monday.Format("2006-01-02")
}

func buildTimeline(buckets map[string]int, weeks int) []DashboardPoint {
	var points []DashboardPoint
	now := time.Now()
	for i := weeks - 1; i >= 0; i-- {
		d := now.AddDate(0, 0, -i*7)
		key := mondayOf(d)
		points = append(points, DashboardPoint{
			Week:  key,
			Count: buckets[key],
		})
	}
	return points
}
