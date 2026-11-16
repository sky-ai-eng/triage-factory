package postgres

import (
	"context"
	"database/sql"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// conversationPendingInputStore is the Postgres impl of db.ConversationPendingInputStore — the
// read half of resume-by-enqueue over the undelivered plain user messages
// (role='user', blank subtype, delivered=false) the ordinary transcript insert
// leaves on the conversation. Peek and Consume run off the admin pool from the
// dispatcher's claim path, a goroutine with no request context; the write they
// read is the follow-up path's InsertMessage through the claims tx, under the
// sending user's claims.
type conversationPendingInputStore struct{ admin queryer }

func newConversationPendingInputStore(admin queryer) db.ConversationPendingInputStore {
	return &conversationPendingInputStore{admin: admin}
}

var _ db.ConversationPendingInputStore = (*conversationPendingInputStore)(nil)

// pendingInputPredicate scopes this store's reads to the row shape it owns:
// the conversation's undelivered plain user messages. The subtype filter
// keeps them disjoint from the staged-injection notes, which are also
// undelivered user rows.
const pendingInputPredicate = `org_id = $1::uuid AND conversation_id = $2::uuid AND role = 'user' AND subtype = '' AND delivered = false`

func (s *conversationPendingInputStore) Peek(ctx context.Context, orgID, conversationID string) (string, string, bool, error) {
	// window_state='active' is not in the shared predicate but belongs in
	// every read of it: a withdrawn row (undelivered + inactive) never
	// happened, and the flush skips it. Without this the routing decision
	// would see input the delivery then cannot find.
	queued, err := scanPendingRows(s.admin.QueryContext(ctx, `
		SELECT id, COALESCE(content, ''), COALESCE(user_id::text, '') FROM messages
		WHERE `+pendingInputPredicate+` AND window_state = 'active'
		ORDER BY id ASC
	`, orgID, conversationID))
	if err == nil {
		queued, err = withStopNotesBetween(ctx, s.admin, orgID, conversationID, queued)
	}
	if err != nil {
		return "", "", false, err
	}
	message, userID, ok := db.JoinPendingRows(queued)
	return message, userID, ok, nil
}

func (s *conversationPendingInputStore) Consume(ctx context.Context, orgID, conversationID string) (string, string, bool, error) {
	// One transaction, so a failed read of the stop notes rolls the flush back
	// and the messages stay waiting, as a failed Consume always leaves them.
	var flushed []db.PendingRow
	err := inTx(ctx, s.admin, func(q queryer) error {
		rows, err := flushPendingInput(ctx, q, orgID, conversationID, []string{""})
		if err != nil {
			return err
		}
		flushed, err = withStopNotesBetween(ctx, q, orgID, conversationID, rows)
		return err
	})
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
		SELECT id, COALESCE(content, ''), COALESCE(user_id::text, '') FROM messages
		WHERE org_id = $1::uuid AND conversation_id = $2::uuid
		  AND role = 'user' AND subtype = $3 AND id > $4 AND id < $5
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

func scanPendingRows(rows *sql.Rows, err error) ([]db.PendingRow, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []db.PendingRow
	for rows.Next() {
		var r db.PendingRow
		if err := rows.Scan(&r.ID, &r.Content, &r.UserID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
