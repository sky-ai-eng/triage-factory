package sqlite

import (
	"context"
	"database/sql"

	"github.com/sky-ai-eng/triage-factory/internal/db"
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
	rows, err := s.q.QueryContext(ctx, `
		SELECT content, user_id FROM messages
		WHERE `+pendingInputPredicate+` AND window_state = 'active'
		ORDER BY id ASC
	`, orgID, conversationID)
	if err != nil {
		return "", "", false, err
	}
	defer rows.Close()
	var queued []pendingRow
	for rows.Next() {
		var (
			r      pendingRow
			userID sql.NullString
		)
		if err := rows.Scan(&r.Content, &userID); err != nil {
			return "", "", false, err
		}
		r.UserID = userID.String
		queued = append(queued, r)
	}
	if err := rows.Err(); err != nil {
		return "", "", false, err
	}
	message, userID, ok := joinPendingRows(queued)
	return message, userID, ok, nil
}

func (s *conversationPendingInputStore) Consume(ctx context.Context, orgID, conversationID string) (string, string, bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", "", false, err
	}
	return consumePendingInput(ctx, s.q, orgID, conversationID)
}
