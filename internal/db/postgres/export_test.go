package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// AssertClaimActiveInTx exposes the claim fence to the external test package,
// so a test can run the fence itself inside a transaction it controls rather
// than a copy of its predicate.
func AssertClaimActiveInTx(ctx context.Context, tx *sql.Tx, orgID, conversationID, claimID string) error {
	return assertClaimActive(ctx, tx, orgID, conversationID, claimID)
}

// ActionObjectIDSQLForTest exposes the SQL twin of domain.ObjectID so the
// ledger conformance suite can hold the two to each other.
const ActionObjectIDSQLForTest = pgActionObjectIDSQL

// PauseNoticesDueQueryForTest and FailureNoticesDueQueryForTest expose the
// delegator-notice reads' statements so the query-plan test can EXPLAIN what
// the store runs.
var (
	PauseNoticesDueQueryForTest   = pauseNoticesDueQuery
	FailureNoticesDueQueryForTest = failureNoticesDueQuery
)

// TrackedEntityIDsForTest returns the ids of the org's entities the
// tracked-set predicate admits, read through q (so under q's RLS, if any):
// every viewer team's when teamIDs is empty, else those teams'. The belt
// reads only db.FactoryBeltSource, so the predicate's other arms, which the
// station counters and the team activity node read, are held to their rules
// through this.
func TrackedEntityIDsForTest(ctx context.Context, q queryer, orgID string, teamIDs []string) (map[string]bool, error) {
	membership := factoryEntityTrackedExists
	args := []any{orgID}
	if len(teamIDs) > 0 {
		ph := make([]string, len(teamIDs))
		for i, id := range teamIDs {
			ph[i] = fmt.Sprintf("$%d", 2+i)
			args = append(args, id)
		}
		membership = factoryEntityTrackedForTeams(strings.Join(ph, ", "))
	}
	rows, err := q.QueryContext(ctx, `SELECT e.id FROM entities e WHERE e.org_id = $1 AND `+membership, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = true
	}
	return ids, rows.Err()
}
