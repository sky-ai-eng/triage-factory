package sqlite

import (
	"context"
	"encoding/json"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// dashboardStore is the SQLite impl of db.DashboardStore. SQL bodies +
// JSON-snapshot scanning logic are ported verbatim from the pre-D2
// internal/db/dashboard.go; behavioral changes are limited to the
// assertLocalOrg guards + context propagation.
//
// The aggregation lives Go-side (not SQL-side) because the source of
// truth is JSON in entities.snapshot_json — SQLite's JSON1 functions
// could extract individual fields but the timeline + per-author
// branching is cleaner as Go. Postgres can revisit using JSONB in a
// later wave; for parity right now, both backends do Go-side aggregation.
type dashboardStore struct{ q queryer }

func newDashboardStore(q queryer) db.DashboardStore { return &dashboardStore{q: q} }

var _ db.DashboardStore = (*dashboardStore)(nil)

func (s *dashboardStore) Stats(ctx context.Context, orgID, username string, sinceDays int) (*domain.DashboardStats, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	since := time.Now().AddDate(0, 0, -sinceDays)

	rows, err := s.q.QueryContext(ctx, `
		SELECT snapshot_json FROM entities
		WHERE source = 'github' AND snapshot_json IS NOT NULL AND snapshot_json != ''
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	stats := &domain.DashboardStats{}
	mergedByDay := make(map[string]int)

	for rows.Next() {
		var snapJSON string
		if err := rows.Scan(&snapJSON); err != nil {
			continue
		}
		var snap domain.PRSnapshot
		if err := json.Unmarshal([]byte(snapJSON), &snap); err != nil {
			// Skip malformed snapshots rather than failing the
			// whole dashboard — one bad row shouldn't 500 the panel.
			continue
		}

		if snap.Author == username {
			switch {
			case snap.Merged:
				mergedAt, err := time.Parse(time.RFC3339, snap.MergedAt)
				if err == nil && mergedAt.After(since) {
					stats.Merged++
					mergedByDay[mergedAt.Format("2006-01-02")]++
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
			for _, review := range snap.Reviews {
				if review.Author == username {
					stats.ReviewsGiven++
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		dashboardLog.Error("stats iteration error", "error", err)
		return nil, err
	}

	stats.MergedOverTime = buildDashboardTimeline(mergedByDay, 14)
	return stats, nil
}

func (s *dashboardStore) PRs(ctx context.Context, orgID, username string) ([]domain.PRSummaryRow, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT snapshot_json FROM entities
		WHERE source = 'github' AND snapshot_json IS NOT NULL AND snapshot_json != ''
		ORDER BY last_polled_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var prs []domain.PRSummaryRow
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

		state := dashboardStateToLower(snap.State)
		if snap.Merged {
			state = "merged"
		}
		prs = append(prs, domain.PRSummaryRow{
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

// dashboardStateToLower preserves the pre-D2 stateToLower() casing
// of PR state strings. Frontend expects lowercase + "merged" as a
// distinct state from "closed", which is mapped by the caller.
func dashboardStateToLower(s string) string {
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

// buildDashboardTimeline reshapes the per-day count map into a
// continuous `days`-bucket slice ending today, filling zeros for
// quiet days. Frontend renders 14 fixed buckets so the sparkline
// stays the same width regardless of activity.
func buildDashboardTimeline(buckets map[string]int, days int) []domain.DashboardPoint {
	points := make([]domain.DashboardPoint, 0, days)
	now := time.Now()
	for i := days - 1; i >= 0; i-- {
		key := now.AddDate(0, 0, -i).Format("2006-01-02")
		points = append(points, domain.DashboardPoint{Date: key, Count: buckets[key]})
	}
	return points
}
