package sqlite

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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

func (s *dashboardStore) Stats(ctx context.Context, orgID, username string, since time.Time) (*domain.DashboardStats, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
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
		// A Scan failure is the driver or the column list disagreeing with
		// this code, not a bad row: skipping it would drop PRs out of a
		// statistic with nothing to show for it, since rows.Err() stays nil.
		if err := rows.Scan(&snapJSON); err != nil {
			return nil, fmt.Errorf("scan pull request snapshot: %w", err)
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
// login). Both legs are SQL predicates, so the window applies to the caller's
// set rather than to the entity table — the Go-side filter this replaced would
// have made page 2 of a 3-row set come back empty whenever a stranger's pull
// request sorted into the first window.
//
// A viewer naming neither identity has no population to answer for and gets an
// empty page rather than the whole install's.
func (s *dashboardStore) PRs(ctx context.Context, orgID string, viewer db.PRViewer, f db.PRListFilter, opts db.ListOpts) ([]domain.PRSummaryRow, int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, 0, err
	}
	stateClause, err := sqlitePRStateClause(f.States)
	if err != nil {
		return nil, 0, err
	}
	// Each leg binds only when the viewer carries that id, so an unbound
	// GitHub identity narrows the list to what they commissioned instead of
	// matching every row whose author is the empty string.
	var args []any
	legs := make([]string, 0, 2)
	if viewer.Login != "" {
		args = append(args, viewer.Login)
		legs = append(legs, `json_extract(e.snapshot_json, '$.author') = ?`)
	}
	if viewer.UserID != "" {
		args = append(args, viewer.UserID)
		legs = append(legs, `e.commissioned_by_user_id = ?`)
	}
	if len(legs) == 0 {
		return []domain.PRSummaryRow{}, 0, nil
	}

	where := `
		WHERE e.source = 'github' AND ` + sqlitePRStateSnapshotGuard + `
		  AND (` + strings.Join(legs, " OR ") + `)` + stateClause

	var total int
	if err := s.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM entities e`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if opts.CountOnly {
		return []domain.PRSummaryRow{}, total, nil
	}

	query := `SELECT e.snapshot_json FROM entities e` + where + sqlitePRSummaryOrder
	if opts.Limit > 0 {
		query += ` LIMIT ? OFFSET ?`
		args = append(args, opts.Limit, opts.Offset)
	}
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	prs, err := sqliteScanPRSummaries(rows)
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
