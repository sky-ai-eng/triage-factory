package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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

func (s *dashboardStore) Stats(ctx context.Context, orgID, username string, since time.Time) (*domain.DashboardStats, error) {
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
		// A Scan failure is the driver or the column list disagreeing with
		// this code, not a bad row: skipping it would drop PRs out of a
		// statistic with nothing to show for it, since rows.Err() stays nil.
		// A malformed snapshot below IS a bad row, and is skipped.
		if err := rows.Scan(&snapJSON); err != nil {
			return nil, fmt.Errorf("scan pull request snapshot: %w", err)
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
					mergedByDay[mergedAt.UTC().Format("2006-01-02")]++
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

// PRs pages the caller's own pull requests: authored under their GitHub login,
// or commissioned by them (a run they asked for opened it under the bot's
// login). Both legs are SQL predicates matching their indexes —
// idx_entities_github_author and idx_entities_commissioned_by — because the
// Go-side filter this replaced read every GitHub entity in the org on every
// dashboard load, and a window over that scan would have paged the wrong set
// (LIMIT applies before the filter, so page 2 could be empty while page 3 has
// rows).
//
// A viewer naming neither identity has no population to answer for and gets an
// empty page rather than the whole org's.
func (s *dashboardStore) PRs(ctx context.Context, orgID string, viewer db.PRViewer, f db.PRListFilter, opts db.ListOpts) ([]domain.PRSummaryRow, int, error) {
	stateClause, err := pgPRStateClause(f.States)
	if err != nil {
		return nil, 0, err
	}
	// Each leg binds only when the viewer carries that id, so an unbound
	// GitHub identity narrows the list to what they commissioned instead of
	// matching every row whose author is the empty string.
	args := []any{orgID}
	legs := make([]string, 0, 2)
	if viewer.Login != "" {
		args = append(args, viewer.Login)
		legs = append(legs, fmt.Sprintf("e.snapshot_json ->> 'author' = $%d", len(args)))
	}
	if viewer.UserID != "" {
		args = append(args, viewer.UserID)
		legs = append(legs, fmt.Sprintf("e.commissioned_by_user_id = $%d", len(args)))
	}
	if len(legs) == 0 {
		return []domain.PRSummaryRow{}, 0, nil
	}

	where := `
		WHERE e.org_id = $1 AND e.source = 'github' AND e.snapshot_json IS NOT NULL
		  AND (` + strings.Join(legs, " OR ") + `)` + stateClause

	var total int
	if err := s.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM entities e`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if opts.CountOnly {
		return []domain.PRSummaryRow{}, total, nil
	}

	query := `SELECT e.snapshot_json FROM entities e` + where + pgPRSummaryOrder
	if opts.Limit > 0 {
		query += fmt.Sprintf(` LIMIT $%d OFFSET $%d`, len(args)+1, len(args)+2)
		args = append(args, opts.Limit, opts.Offset)
	}
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	prs, err := pgScanPRSummaries(rows)
	if err != nil {
		return nil, 0, err
	}
	return prs, total, nil
}

// buildDashboardTimeline reshapes the per-day count map into a
// continuous `days`-bucket slice ending today, filling zeros for
// quiet days. Frontend renders 14 fixed buckets so the sparkline
// stays the same width regardless of activity.
//
// The bucket keys are UTC days, because that is what the counting side
// produces: the merged-at timestamps come off the snapshot as UTC. Built in
// the process's local zone instead, the two sides name different days for
// every instant within the offset of midnight, and the sparkline silently
// reads zero for activity it counted.
func buildDashboardTimeline(buckets map[string]int, days int) []domain.DashboardPoint {
	points := make([]domain.DashboardPoint, 0, days)
	now := time.Now().UTC()
	for i := days - 1; i >= 0; i-- {
		key := now.AddDate(0, 0, -i).Format("2006-01-02")
		points = append(points, domain.DashboardPoint{Date: key, Count: buckets[key]})
	}
	return points
}
