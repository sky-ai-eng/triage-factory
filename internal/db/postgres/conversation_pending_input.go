package postgres

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// runPendingInputStore is the Postgres impl of db.ConversationPendingInputStore — the
// read half of resume-by-enqueue over the undelivered plain user messages
// (role='user', blank subtype, delivered=false) the ordinary transcript insert
// leaves on the conversation. Peek and Consume run off the admin pool from the
// dispatcher's claim path, a goroutine with no request context; the write they
// read is the follow-up path's InsertMessage through the claims tx, under the
// sending user's claims.
type runPendingInputStore struct{ admin queryer }

func newConversationPendingInputStore(admin queryer) db.ConversationPendingInputStore {
	return &runPendingInputStore{admin: admin}
}

var _ db.ConversationPendingInputStore = (*runPendingInputStore)(nil)

// pendingInputPredicate scopes this store's reads to the row shape it owns:
// the conversation's undelivered plain user messages. The subtype filter
// keeps them disjoint from the staged-injection notes, which are also
// undelivered user rows.
const pendingInputPredicate = `org_id = $1::uuid AND conversation_id = $2::uuid AND role = 'user' AND subtype = '' AND delivered = false`

func (s *runPendingInputStore) Peek(ctx context.Context, orgID, conversationID string) (string, string, bool, error) {
	// window_state='active' is not in the shared predicate but belongs in
	// every read of it: a withdrawn row (undelivered + inactive) never
	// happened, and the flush skips it. Without this the routing decision
	// would see input the delivery then cannot find.
	rows, err := s.admin.QueryContext(ctx, `
		SELECT content, COALESCE(user_id::text, '') FROM messages
		WHERE `+pendingInputPredicate+` AND window_state = 'active'
		ORDER BY id ASC
	`, orgID, conversationID)
	if err != nil {
		return "", "", false, err
	}
	defer rows.Close()
	var queued []pendingRow
	for rows.Next() {
		var r pendingRow
		if err := rows.Scan(&r.Content, &r.UserID); err != nil {
			return "", "", false, err
		}
		queued = append(queued, r)
	}
	if err := rows.Err(); err != nil {
		return "", "", false, err
	}
	message, userID, ok := joinPendingRows(queued)
	return message, userID, ok, nil
}

func (s *runPendingInputStore) Consume(ctx context.Context, orgID, conversationID string) (string, string, bool, error) {
	return consumePendingInput(ctx, s.admin, orgID, conversationID)
}
