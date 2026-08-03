package sqlite

import (
	"context"
	"database/sql"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// runPendingInputStore is the SQLite impl of db.RunPendingInputStore — the
// durable half of resume-by-enqueue (TFAC-585), stored as undelivered plain
// user messages (role='user', blank subtype, delivered=0) on the
// conversation's own transcript. SQLite is N=1, no RLS; org_id exists for
// parity with the Postgres baseline and every caller passes
// LocalDefaultOrgID (asserted at each entry).
type runPendingInputStore struct{ q queryer }

func newRunPendingInputStore(q queryer) db.RunPendingInputStore {
	return &runPendingInputStore{q: q}
}

var _ db.RunPendingInputStore = (*runPendingInputStore)(nil)

// pendingInputPredicate scopes this store's reads to the row shape it owns:
// the conversation's undelivered plain user messages. The subtype filter
// keeps them disjoint from the staged-injection notes, which are also
// undelivered user rows.
const pendingInputPredicate = `org_id = ? AND conversation_id = ? AND role = 'user' AND subtype = '' AND delivered = 0`

func (s *runPendingInputStore) Store(ctx context.Context, orgID, runID, userID, message string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	// A plain insert: the queue appends. Two messages sent to a parked
	// conversation before it wakes are two rows, and Consume joins them —
	// the delete-then-insert this replaced dropped the first one silently.
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO messages (org_id, conversation_id, user_id, role, content, subtype, delivered)
		VALUES (?, ?, ?, 'user', ?, '', 0)
	`, orgID, runID, sqliteNullStr(userID), message)
	return err
}

func (s *runPendingInputStore) Peek(ctx context.Context, orgID, runID string) (string, string, bool, error) {
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
	`, orgID, runID)
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

func (s *runPendingInputStore) Consume(ctx context.Context, orgID, runID string) (string, string, bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", "", false, err
	}
	return consumePendingInput(ctx, s.q, orgID, runID)
}
