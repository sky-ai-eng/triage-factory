package sqlite

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
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
func flushPendingInput(ctx context.Context, q queryer, orgID, convID, subtype string) ([]pendingRow, error) {
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
	var out []pendingRow
	for rows.Next() {
		var (
			r        pendingRow
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

// consumePendingInput is the resume path's view of the flush: a parked
// conversation's plain user input, delivered and handed back. It is the same
// primitive every other pending row goes through — the empty subtype is what
// discriminates "the user's next message" from an injection.
//
// Every flushed row is returned, joined, because every flushed row is a
// message a person sent. The flush marks them all delivered whatever this
// does with them, so returning one and dropping the rest is how they would be
// lost silently.
func consumePendingInput(ctx context.Context, q queryer, orgID, convID string) (message, userID string, ok bool, err error) {
	flushed, err := flushPendingInput(ctx, q, orgID, convID, "")
	if err != nil {
		return "", "", false, err
	}
	message, userID, ok = joinPendingRows(flushed)
	return message, userID, ok, nil
}

// joinPendingRows assembles queued input into the single prompt string an SDK
// resume takes: every row, oldest first, separated by a blank line. Peek and
// Consume both go through it, which is what keeps the routing decision and
// the delivery from disagreeing about what is pending.
//
// The userID is the NEWEST row's — see db.RunPendingInputStore for why a
// queue with several authors reports one, and why it is that one.
func joinPendingRows(rows []pendingRow) (message, userID string, ok bool) {
	if len(rows) == 0 {
		return "", "", false
	}
	parts := make([]string, 0, len(rows))
	for _, r := range rows {
		parts = append(parts, r.Content)
	}
	return strings.Join(parts, db.PendingInputSeparator), rows[len(rows)-1].UserID, true
}
