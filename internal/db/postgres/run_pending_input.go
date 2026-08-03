package postgres

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// runPendingInputStore is the Postgres impl of db.RunPendingInputStore —
// the durable half of resume-by-enqueue, stored as undelivered plain user
// messages (role='user', blank subtype, delivered=false) on the
// conversation's own transcript. Reachable two ways: the resume path writes
// through the claims tx, under the resuming user's claims (the messages RLS
// policy admits the write via the conversation's own visibility); Consume
// runs off the admin pool from the dispatcher's claim path, a goroutine with
// no request context.
type runPendingInputStore struct{ admin queryer }

func newRunPendingInputStore(admin queryer) db.RunPendingInputStore {
	return &runPendingInputStore{admin: admin}
}

var _ db.RunPendingInputStore = (*runPendingInputStore)(nil)

// pendingInputPredicate scopes this store's reads to the row shape it owns:
// the conversation's undelivered plain user messages. The subtype filter
// keeps them disjoint from the staged-injection notes, which are also
// undelivered user rows.
const pendingInputPredicate = `org_id = $1::uuid AND conversation_id = $2::uuid AND role = 'user' AND subtype = '' AND delivered = false`

func (s *runPendingInputStore) Store(ctx context.Context, orgID, runID, userID, message string) error {
	// A plain insert: the queue appends. Two messages sent to a parked
	// conversation before it wakes are two rows, and Consume joins them —
	// the delete-then-insert this replaced dropped the first one silently.
	_, err := s.admin.ExecContext(ctx, `
		INSERT INTO messages (org_id, conversation_id, user_id, role, content, subtype, delivered)
		VALUES ($1::uuid, $2::uuid, NULLIF($3, '')::uuid, 'user', $4, '', false)
	`, orgID, runID, userID, message)
	return err
}

func (s *runPendingInputStore) Peek(ctx context.Context, orgID, runID string) (string, string, bool, error) {
	// window_state='active' is not in the shared predicate but belongs in
	// every read of it: a withdrawn row (undelivered + inactive) never
	// happened, and the flush skips it. Without this the routing decision
	// would see input the delivery then cannot find.
	rows, err := s.admin.QueryContext(ctx, `
		SELECT content, COALESCE(user_id::text, '') FROM messages
		WHERE `+pendingInputPredicate+` AND window_state = 'active'
		ORDER BY id ASC
	`, orgID, runID)
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

func (s *runPendingInputStore) Consume(ctx context.Context, orgID, runID string) (string, string, bool, error) {
	return consumePendingInput(ctx, s.admin, orgID, runID)
}
