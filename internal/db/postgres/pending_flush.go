package postgres

import (
	"context"
	"time"
)

// pendingRow is one row the flush claimed: enough for every consumer without
// any of them re-spelling the query.
type pendingRow struct {
	ID        int64
	OrgID     string
	ConvID    string
	Content   string
	UserID    string
	Metadata  string
	CreatedAt time.Time
}

// flushPendingInput is THE flush primitive. Every surface's pending input —
// a parked run's resume message, a staged system note, a curator turn — is
// the same thing in the same table: an undelivered messages row. So there is
// one statement that consumes them, parameterized by nothing but the subtype
// that discriminates the kind, rather than one query per producer.
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
func flushPendingInput(ctx context.Context, q queryer, orgID, convID, subtype string) ([]pendingRow, error) {
	rows, err := q.QueryContext(ctx, `
		WITH flushed AS (
			UPDATE messages SET delivered = true
			WHERE org_id = $1::uuid AND conversation_id = $2::uuid
			  AND delivered = false AND window_state = 'active'
			  AND role = 'user' AND subtype = $3
			RETURNING id, org_id, conversation_id, content, user_id, metadata, created_at
		)
		SELECT id, org_id::text, conversation_id::text, content,
		       COALESCE(user_id::text, ''), COALESCE(metadata::text, ''), created_at
		FROM flushed ORDER BY id ASC
	`, orgID, convID, subtype)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pendingRow
	for rows.Next() {
		var r pendingRow
		if err := rows.Scan(&r.ID, &r.OrgID, &r.ConvID, &r.Content, &r.UserID, &r.Metadata, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// consumePendingInput is the resume path's view of the flush: a parked
// conversation's plain user input, delivered and handed back. It is the same
// primitive every other pending row goes through — the empty subtype is what
// discriminates "the user's next message" from an injection. Store keeps at
// most one such row per conversation, so the newest is the one; taking the
// newest rather than the oldest matches Peek, which routes off it.
func consumePendingInput(ctx context.Context, q queryer, orgID, convID string) (message, userID string, ok bool, err error) {
	flushed, err := flushPendingInput(ctx, q, orgID, convID, "")
	if err != nil || len(flushed) == 0 {
		return "", "", false, err
	}
	newest := flushed[len(flushed)-1]
	return newest.Content, newest.UserID, true, nil
}
