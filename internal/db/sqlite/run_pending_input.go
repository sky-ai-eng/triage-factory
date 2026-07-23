package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// runPendingInputStore is the SQLite impl of db.RunPendingInputStore — the
// durable half of resume-by-enqueue (TFAC-585), stored as an undelivered
// plain user message (role='user', subtype='text', delivered=0) on the
// conversation's own transcript. SQLite is N=1, no RLS; org_id exists for
// parity with the Postgres baseline and every caller passes
// LocalDefaultOrgID (asserted at each entry).
type runPendingInputStore struct{ q queryer }

func newRunPendingInputStore(q queryer) db.RunPendingInputStore {
	return &runPendingInputStore{q: q}
}

var _ db.RunPendingInputStore = (*runPendingInputStore)(nil)

// pendingInputPredicate scopes every read/write to the one row shape this
// store owns: the conversation's undelivered plain user message. The
// subtype filter keeps it disjoint from the staged-injection notes, which
// are also undelivered user rows.
const pendingInputPredicate = `org_id = ? AND conversation_id = ? AND role = 'user' AND subtype = 'text' AND delivered = 0`

func (s *runPendingInputStore) Store(ctx context.Context, orgID, runID, userID, message string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	// Delete-then-insert preserves the replace contract the former
	// run_pending_input upsert had: at most one undelivered user row per
	// conversation, latest write wins.
	return inTx(ctx, s.q, func(q queryer) error {
		if _, err := q.ExecContext(ctx, `
			DELETE FROM messages WHERE `+pendingInputPredicate+`
		`, orgID, runID); err != nil {
			return err
		}
		_, err := q.ExecContext(ctx, `
			INSERT INTO messages (org_id, conversation_id, user_id, role, content, subtype, delivered)
			VALUES (?, ?, ?, 'user', ?, 'text', 0)
		`, orgID, runID, sqliteNullStr(userID), message)
		return err
	})
}

func (s *runPendingInputStore) Peek(ctx context.Context, orgID, runID string) (string, string, bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", "", false, err
	}
	var message string
	var userID sql.NullString
	err := s.q.QueryRowContext(ctx, `
		SELECT content, user_id FROM messages WHERE `+pendingInputPredicate+`
		ORDER BY id DESC LIMIT 1
	`, orgID, runID).Scan(&message, &userID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return message, userID.String, true, nil
}

func (s *runPendingInputStore) Consume(ctx context.Context, orgID, runID string) (string, string, bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", "", false, err
	}
	var message string
	var userID sql.NullString
	err := s.q.QueryRowContext(ctx, `
		UPDATE messages SET delivered = 1 WHERE `+pendingInputPredicate+`
		RETURNING content, user_id
	`, orgID, runID).Scan(&message, &userID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return message, userID.String, true, nil
}
