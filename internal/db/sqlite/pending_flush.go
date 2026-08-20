package sqlite

import (
	"context"
	"database/sql"
	"sort"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// flushPendingInput is THE flush primitive — the SQLite twin of the Postgres
// one. Every surface's pending input is the same thing in the same table: an
// undelivered messages row, so one statement consumes them all,
// parameterized by nothing but the subtype that discriminates the kind.
//
// UPDATE … RETURNING in a single statement is what makes a row delivered
// exactly once; SQLite does not honor ORDER BY on RETURNING, so oldest-first
// is restored Go-side by the monotonic row id. window_state='active' keeps
// withdrawn rows out — withdrawn means "never happened" — matching the claim
// predicate's own view of drivable input.
func flushPendingInput(ctx context.Context, q queryer, orgID, convID, subtype string) ([]db.PendingRow, error) {
	rows, err := q.QueryContext(ctx, `
		UPDATE messages SET delivered = 1
		WHERE org_id = ? AND conversation_id = ?
		  AND delivered = 0 AND window_state = 'active'
		  AND role = 'user' AND subtype = ?
		RETURNING id, org_id, conversation_id, content, user_id, metadata, created_at
	`, orgID, convID, subtype)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []db.PendingRow
	for rows.Next() {
		var (
			r        db.PendingRow
			userID   sql.NullString
			metadata sql.NullString
		)
		if err := rows.Scan(&r.ID, &r.OrgID, &r.ConvID, &r.Content, &userID, &metadata, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.UserID, r.Metadata = userID.String, metadata.String
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
