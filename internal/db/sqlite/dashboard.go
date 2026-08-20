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

// PRs pages the caller's authored PRs. The author predicate is
// json_extract(snapshot_json, '$.author'), matching the expression index of
// the same shape — the Go-side filter it replaced read every GitHub entity in
// the database on every dashboard load, and a window over that scan would
// have paged the wrong set (LIMIT applies before the filter, so page 2 could
// be empty while page 3 has rows).
func (s *dashboardStore) PRs(ctx context.Context, orgID, username string, opts db.ListOpts) ([]domain.PRSummaryRow, int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, 0, err
	}
	// json_valid() guards the extract: this dialect stores the snapshot as TEXT,
	// so an unparseable value can exist, and json_extract() on one raises
	// "malformed JSON" — which would turn a single bad row into a 500 for the
	// whole dashboard. AND short-circuits, and the expression index carries the
	// same predicate, so the guard costs nothing and the index still serves the
	// read. The Go decode below skips a bad row for the same reason.
	const where = `
		WHERE source = 'github' AND snapshot_json IS NOT NULL AND snapshot_json != ''
		  AND json_valid(snapshot_json)
		  AND json_extract(snapshot_json, '$.author') = ?`

	var total int
	if err := s.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM entities`+where, username).Scan(&total); err != nil {
		return nil, 0, err
	}

	query := `SELECT snapshot_json FROM entities` + where + `
		ORDER BY last_polled_at DESC, id`
	args := []any{username}
	if opts.Limit > 0 {
		query += ` LIMIT ? OFFSET ?`
		args = append(args, opts.Limit, opts.Offset)
	}
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	prs := []domain.PRSummaryRow{}
	for rows.Next() {
		var snapJSON string
		if err := rows.Scan(&snapJSON); err != nil {
			continue
		}
		var snap domain.PRSnapshot
		if err := json.Unmarshal([]byte(snapJSON), &snap); err != nil {
			continue
		}
		prs = append(prs, domain.PRSummaryFromSnapshot(snap))
	}
	return prs, total, rows.Err()
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
