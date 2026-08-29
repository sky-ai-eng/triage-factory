package sqlite

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// The pieces both pull-request list arms share: the states filter's SQL and
// the snapshot → row projection. Same split as the Postgres twin, and the two
// vocabularies have to mean the same thing or a filter would answer
// differently per mode.

// sqlitePRStateSnapshotGuard is the json_valid() gate every snapshot-reading
// predicate sits behind. This dialect stores the snapshot as TEXT, so an
// unparseable value can exist, and json_extract() on one raises "malformed
// JSON" — which would turn a single bad row into a 500 for the whole list. AND
// short-circuits, and the author index carries the same predicate, so the
// guard costs nothing and the index still serves the read.
const sqlitePRStateSnapshotGuard = `e.snapshot_json IS NOT NULL AND e.snapshot_json != '' AND json_valid(e.snapshot_json)`

// sqlitePRStatePredicates maps the wire vocabulary to fixed SQL against an
// entities row aliased `e`. The caller's string never reaches the statement —
// it selects a fragment or it is refused.
//
// open reads the entity's own state; merged and closed read the snapshot, with
// both of GitHub's spellings of merged landing as merged. json_extract returns
// SQLite's 1/0 for a JSON boolean, which is what the merged arms compare
// against.
var sqlitePRStatePredicates = map[string]string{
	domain.PRStateOpen:   `e.state = 'active'`,
	domain.PRStateMerged: `(json_extract(e.snapshot_json, '$.state') = 'MERGED' OR json_extract(e.snapshot_json, '$.merged') = 1)`,
	domain.PRStateClosed: `(json_extract(e.snapshot_json, '$.state') = 'CLOSED' AND IFNULL(json_extract(e.snapshot_json, '$.merged'), 0) <> 1)`,
}

// sqlitePRStateClause renders the states filter as a trailing AND-ed
// disjunction, or "" for the unfiltered case. An unknown state is
// db.ErrUnknownPRState rather than a silently widened result set.
func sqlitePRStateClause(states []string) (string, error) {
	if len(states) == 0 {
		return "", nil
	}
	arms := make([]string, 0, len(states))
	for _, st := range states {
		frag, ok := sqlitePRStatePredicates[st]
		if !ok {
			return "", fmt.Errorf("%w: %q", db.ErrUnknownPRState, st)
		}
		arms = append(arms, frag)
	}
	return "\n\t\t  AND (" + strings.Join(arms, " OR ") + ")", nil
}

// sqlitePRSummaryOrder is the total order both arms page by. The id
// tiebreaker is required: without it, rows sharing a last_polled_at drop and
// repeat across offset pages.
const sqlitePRSummaryOrder = `
	ORDER BY e.last_polled_at DESC, e.id`

// sqliteScanPRSummaries drains a snapshot_json-only result set into rows. A
// malformed snapshot is skipped rather than failing the page — the same reason
// the guard above exists.
func sqliteScanPRSummaries(rows *sql.Rows) ([]domain.PRSummaryRow, error) {
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
	return prs, rows.Err()
}
