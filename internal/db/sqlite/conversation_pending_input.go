package sqlite

import (
	"context"
	"database/sql"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// conversationPendingInputStore is the SQLite impl of db.ConversationPendingInputStore — the
// read half of resume-by-enqueue over the undelivered plain user messages
// (role='user', blank subtype, delivered=0) the ordinary transcript insert
// leaves on the conversation. SQLite is N=1, no RLS; org_id exists for parity
// with the Postgres baseline and every caller passes LocalDefaultOrgID
// (asserted at each entry).
type conversationPendingInputStore struct{ q queryer }

func newConversationPendingInputStore(q queryer) db.ConversationPendingInputStore {
	return &conversationPendingInputStore{q: q}
}

var _ db.ConversationPendingInputStore = (*conversationPendingInputStore)(nil)

// pendingInputPredicate scopes this store's reads to the row shape it owns:
// the conversation's undelivered plain user messages. The subtype filter
// keeps them disjoint from the staged-injection notes, which are also
// undelivered user rows.
const pendingInputPredicate = `org_id = ? AND conversation_id = ? AND role = 'user' AND subtype = '' AND delivered = 0`

func (s *conversationPendingInputStore) Peek(ctx context.Context, orgID, conversationID string) (string, string, bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", "", false, err
	}
	// window_state='active' is not in the shared predicate but belongs in
	// every read of it: a withdrawn row (undelivered + inactive) never
	// happened, and the flush skips it. Without this the routing decision
	// would see input the delivery then cannot find.
	queued, err := scanPendingRows(s.q.QueryContext(ctx, `
		SELECT id, content, user_id FROM messages
		WHERE `+pendingInputPredicate+` AND window_state = 'active'
		ORDER BY id ASC
	`, orgID, conversationID))
	if err == nil {
		queued, err = withStopNotesBetween(ctx, s.q, orgID, conversationID, queued)
	}
	if err != nil {
		return "", "", false, err
	}
	message, userID, ok := db.JoinPendingRows(queued)
	return message, userID, ok, nil
}

func (s *conversationPendingInputStore) Consume(ctx context.Context, orgID, conversationID string) (string, string, bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", "", false, err
	}
	flushed, err := flushPendingInput(ctx, s.q, orgID, conversationID, []string{""})
	if err == nil {
		flushed, err = withStopNotesBetween(ctx, s.q, orgID, conversationID, flushed)
	}
	return db.ConsumePendingInput(flushed, err)
}

// withStopNotesBetween adds to queued the conversation's stop-note rows that
// lie between the oldest and the newest of its person messages, so the
// resume message reads each stop where it fell (db.JoinPendingRows). The
// notes were delivered when they were written, and are only read here.
func withStopNotesBetween(ctx context.Context, q queryer, orgID, conversationID string, queued []db.PendingRow) ([]db.PendingRow, error) {
	lo, hi, ok := db.PendingRowBounds(queued)
	if !ok {
		return queued, nil
	}
	notes, err := scanPendingRows(q.QueryContext(ctx, `
		SELECT id, content, user_id FROM messages
		WHERE org_id = ? AND conversation_id = ?
		  AND role = 'user' AND subtype = ? AND id > ? AND id < ?
		ORDER BY id ASC
	`, orgID, conversationID, domain.MessageSubtypeStopNote, lo, hi))
	if err != nil {
		return nil, err
	}
	for i := range notes {
		notes[i].Subtype = domain.MessageSubtypeStopNote
	}
	return append(queued, notes...), nil
}

// scanPendingRows reads (id, content, user_id) rows into PendingRows.
func scanPendingRows(rows *sql.Rows, err error) ([]db.PendingRow, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []db.PendingRow
	for rows.Next() {
		var (
			r       db.PendingRow
			content sql.NullString
			userID  sql.NullString
		)
		if err := rows.Scan(&r.ID, &content, &userID); err != nil {
			return nil, err
		}
		r.Content, r.UserID = content.String, userID.String
		out = append(out, r)
	}
	return out, rows.Err()
}
