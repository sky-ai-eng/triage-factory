package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// dashboardStore is the Postgres impl of db.DashboardStore. Aggregation
// lives Go-side, identical to SQLite — entities.snapshot_json is the
// source of truth for PR state and the per-author / per-day filtering
// is cleaner in Go than as a JSONB monster. JSONB-native rewrites can
// happen in a later wave; for now both backends share the assertion
// surface in dbtest.
type dashboardStore struct{ q queryer }

func newDashboardStore(q queryer) db.DashboardStore { return &dashboardStore{q: q} }

var _ db.DashboardStore = (*dashboardStore)(nil)

func (s *dashboardStore) Stats(ctx context.Context, orgID, username string, sinceDays int) (*domain.DashboardStats, error) {
	since := time.Now().AddDate(0, 0, -sinceDays)

	rows, err := s.q.QueryContext(ctx, `
		SELECT snapshot_json FROM entities
		WHERE org_id = $1 AND source = 'github' AND snapshot_json IS NOT NULL
	`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	stats := &domain.DashboardStats{}
	mergedByDay := make(map[string]int)

	for rows.Next() {
		var snapJSON []byte
		if err := rows.Scan(&snapJSON); err != nil {
			continue
		}
		if len(snapJSON) == 0 {
			continue
		}
		var snap domain.PRSnapshot
		if err := json.Unmarshal(snapJSON, &snap); err != nil {
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
	rows, err := s.q.QueryContext(ctx, `
		SELECT snapshot_json FROM entities
		WHERE org_id = $1 AND source = 'github' AND snapshot_json IS NOT NULL
		ORDER BY last_polled_at DESC NULLS LAST
	`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var prs []domain.PRSummaryRow
	for rows.Next() {
		var snapJSON []byte
		if err := rows.Scan(&snapJSON); err != nil {
			continue
		}
		if len(snapJSON) == 0 {
			continue
		}
		var snap domain.PRSnapshot
		if err := json.Unmarshal(snapJSON, &snap); err != nil {
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

// dashboardStateToLower is the per-D2 mirror of the SQLite helper.
// Lives in this package (vs a shared internals package) because
// duplicating four lines is cheaper than introducing a "shared
// helpers" import dependency for one trivial mapping.
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

func buildDashboardTimeline(buckets map[string]int, days int) []domain.DashboardPoint {
	points := make([]domain.DashboardPoint, 0, days)
	now := time.Now()
	for i := days - 1; i >= 0; i-- {
		key := now.AddDate(0, 0, -i).Format("2006-01-02")
		points = append(points, domain.DashboardPoint{Date: key, Count: buckets[key]})
	}
	return points
}
