package postgres

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// The pieces both pull-request list arms share: the states filter's SQL and
// the snapshot → row projection. They live together because the two arms
// differ only in whose pull requests they select, and a states filter that
// meant one thing on the personal list and another on the team list would be
// the same word answering two questions.

// pgPRStatePredicates maps the wire vocabulary to fixed SQL against an
// entities row aliased `e`. The caller's string never reaches the statement —
// it selects a fragment or it is refused — so an unvalidated value cannot
// become SQL.
//
// open reads the ENTITY's own state, not the snapshot's: it is the population
// the factory belt renders, so the Overview's OPEN PRS figure and the belt
// cannot disagree. merged and closed read the snapshot, where GitHub's two
// wire shapes for a merged pull request (MERGED, or CLOSED with merged=true)
// both have to land as merged — which is also what PRSummaryFromSnapshot does
// to the row's own state field, so a filtered page reads back the state it
// was filtered by.
var pgPRStatePredicates = map[string]string{
	domain.PRStateOpen:   `e.state = 'active'`,
	domain.PRStateMerged: `(e.snapshot_json ->> 'state' = 'MERGED' OR e.snapshot_json ->> 'merged' = 'true')`,
	domain.PRStateClosed: `(e.snapshot_json ->> 'state' = 'CLOSED' AND coalesce(e.snapshot_json ->> 'merged', 'false') <> 'true')`,
}

// pgPRStateClause renders the states filter as a trailing AND-ed disjunction,
// or "" for the unfiltered case. An unknown state is db.ErrUnknownPRState:
// dropping it would widen the result set behind a filter the caller believes
// is narrowing it.
func pgPRStateClause(states []string) (string, error) {
	if len(states) == 0 {
		return "", nil
	}
	arms := make([]string, 0, len(states))
	for _, st := range states {
		frag, ok := pgPRStatePredicates[st]
		if !ok {
			return "", fmt.Errorf("%w: %q", db.ErrUnknownPRState, st)
		}
		arms = append(arms, frag)
	}
	return "\n\t\t  AND (" + strings.Join(arms, " OR ") + ")", nil
}

// pgPRSummaryOrder is the total order both arms page by. The id tiebreaker is
// required, not decorative: without it, rows sharing a last_polled_at drop and
// repeat across offset pages.
const pgPRSummaryOrder = `
	ORDER BY e.last_polled_at DESC NULLS LAST, e.id`

// pgScanPRSummaries drains a snapshot_json-only result set into rows.
//
// The two failure modes are deliberately not the same. A malformed or empty
// snapshot is one bad stored row, skipped so it cannot blank a whole list. A
// Scan failure is not a row problem at all — it is the driver or the column
// list disagreeing with this code — so it fails the read: skipping it would
// silently serve a short page that rows.Err() reports nothing about, which is
// a wrong answer wearing a right one's shape.
func pgScanPRSummaries(rows *sql.Rows) ([]domain.PRSummaryRow, error) {
	defer rows.Close()
	prs := []domain.PRSummaryRow{}
	for rows.Next() {
		var snapJSON []byte
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
		prs = append(prs, domain.PRSummaryFromSnapshot(snap))
	}
	return prs, rows.Err()
}
