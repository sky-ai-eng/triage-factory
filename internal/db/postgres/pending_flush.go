package postgres

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// flushPendingInput is THE flush primitive. Every surface's pending input —
// a parked run's resume message, a staged system note — is the same thing
// in the same table: an undelivered messages row. So there is
// one statement that consumes them, parameterized by nothing but the subtypes
// that discriminate the kind, rather than one query per producer.
//
// UPDATE … RETURNING in a single statement is what makes a row delivered
// exactly once under a race; the id sort restores oldest-first, which
// RETURNING does not promise. window_state='active' keeps withdrawn rows
// (undelivered + inactive) out — withdrawn means "never happened" — and
// matches the claim predicate's own view of what counts as drivable input,
// so the flush can never consume something the loop would not have woken
// for.
//
// Callers pass an admin-pool queryer: both producers and both consumers run
// in background goroutines with no JWT claims, so an app-pool statement
// would see nothing. org_id stays bound as defense in depth.
func flushPendingInput(ctx context.Context, q queryer, orgID, convID string, subtypes []string) ([]db.PendingRow, error) {
	rows, err := q.QueryContext(ctx, `
		WITH flushed AS (
			UPDATE messages SET delivered = true
			WHERE org_id = $1::uuid AND conversation_id = $2::uuid
			  AND delivered = false AND window_state = 'active'
			  AND role = 'user' AND subtype = ANY($3::text[])
			RETURNING id, org_id, conversation_id, content, subtype, user_id, metadata, created_at
		)
		SELECT id, org_id::text, conversation_id::text, content, subtype,
		       COALESCE(user_id::text, ''), COALESCE(metadata::text, ''), created_at
		FROM flushed ORDER BY id ASC
	`, orgID, convID, subtypes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []db.PendingRow
	for rows.Next() {
		var r db.PendingRow
		if err := rows.Scan(&r.ID, &r.OrgID, &r.ConvID, &r.Content, &r.Subtype, &r.UserID, &r.Metadata, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
