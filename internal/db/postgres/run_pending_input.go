package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// runPendingInputStore is the Postgres impl of db.RunPendingInputStore —
// the durable half of resume-by-enqueue (TFAC-585), stored as an
// undelivered plain user message (role='user', subtype='text',
// delivered=false) on the conversation's own transcript. Reachable two
// ways: the resume flip binds it to the claims tx so Store commits
// atomically with the status flip under the resuming user's claims (the
// messages RLS policy admits the write via the conversation's own
// visibility); Consume runs off the admin pool from the dispatcher's claim
// path, a goroutine with no request context.
type runPendingInputStore struct{ admin queryer }

func newRunPendingInputStore(admin queryer) db.RunPendingInputStore {
	return &runPendingInputStore{admin: admin}
}

var _ db.RunPendingInputStore = (*runPendingInputStore)(nil)

// pendingInputPredicate scopes every read/write to the one row shape this
// store owns: the conversation's undelivered plain user message. The
// subtype filter keeps it disjoint from the staged-injection notes, which
// are also undelivered user rows.
const pendingInputPredicate = `org_id = $1::uuid AND conversation_id = $2::uuid AND role = 'user' AND subtype = 'text' AND delivered = false`

func (s *runPendingInputStore) Store(ctx context.Context, orgID, runID, userID, message string) error {
	// Delete-then-insert preserves the replace contract the former
	// run_pending_input upsert had: at most one undelivered user row per
	// conversation, latest write wins.
	return inTx(ctx, s.admin, func(q queryer) error {
		if _, err := q.ExecContext(ctx, `
			DELETE FROM messages WHERE `+pendingInputPredicate+`
		`, orgID, runID); err != nil {
			return err
		}
		_, err := q.ExecContext(ctx, `
			INSERT INTO messages (org_id, conversation_id, user_id, role, content, subtype, delivered)
			VALUES ($1::uuid, $2::uuid, NULLIF($3, '')::uuid, 'user', $4, 'text', false)
		`, orgID, runID, userID, message)
		return err
	})
}

func (s *runPendingInputStore) Peek(ctx context.Context, orgID, runID string) (string, string, bool, error) {
	var message string
	var userID sql.NullString
	err := s.admin.QueryRowContext(ctx, `
		SELECT content, user_id::text FROM messages
		WHERE `+pendingInputPredicate+`
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
	var message string
	var userID sql.NullString
	err := s.admin.QueryRowContext(ctx, `
		UPDATE messages SET delivered = true
		WHERE `+pendingInputPredicate+`
		RETURNING content, user_id::text
	`, orgID, runID).Scan(&message, &userID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return message, userID.String, true, nil
}
