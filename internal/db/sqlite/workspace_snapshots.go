package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// workspaceSnapshotStore is the SQLite impl of db.WorkspaceSnapshotStore — the
// per-snapshot-key lifecycle record. N=1, no RLS, so the admin/app split
// collapses onto the one connection; assertLocalOrg gates every entry point.
//
// The single-process case still needs the row: a local park writes its blob
// after the status flip just as an executor does, so "pending with a live
// writer" is the same answer here as in a fleet, and a restart-crossing failed
// write is the same durable "stop waiting".
type workspaceSnapshotStore struct{ q queryer }

func newWorkspaceSnapshotStore(q queryer) db.WorkspaceSnapshotStore {
	return &workspaceSnapshotStore{q: q}
}

var _ db.WorkspaceSnapshotStore = (*workspaceSnapshotStore)(nil)

func (s *workspaceSnapshotStore) BeginSnapshotSystem(ctx context.Context, orgID, taskID, claimID string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	// The refusal orders claims the way every "newest claim" read on this
	// dialect does: claimed_at, then rowid for two minted in one instant.
	res, err := s.q.ExecContext(ctx, `
		INSERT INTO workspace_snapshots (org_id, task_id, state, writer_claim_id, updated_at)
		VALUES (?, ?, 'pending', ?, ?)
		ON CONFLICT(org_id, task_id) DO UPDATE SET
			state               = excluded.state,
			writer_claim_id     = excluded.writer_claim_id,
			updated_at          = excluded.updated_at,
			covered_position    = NULL,
			covered_fingerprint = NULL
		WHERE NOT EXISTS (
			SELECT 1 FROM claims cur, claims mine
			WHERE cur.id  = workspace_snapshots.writer_claim_id
			  AND mine.id = ?
			  AND cur.id <> mine.id
			  AND (cur.claimed_at, cur.rowid) > (mine.claimed_at, mine.rowid)
		)
	`, orgID, taskID, claimID, time.Now().UTC(), claimID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return db.ErrSnapshotSuperseded
	}
	return nil
}

func (s *workspaceSnapshotStore) FinishSnapshotSystem(ctx context.Context, orgID, taskID, claimID string, ok bool) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	state := domain.WorkspaceSnapshotFailed
	if ok {
		state = domain.WorkspaceSnapshotWritten
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE workspace_snapshots
		SET state = ?, updated_at = ?
		WHERE org_id = ? AND task_id = ? AND writer_claim_id = ? AND state = 'pending'
	`, state, time.Now().UTC(), orgID, taskID, claimID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *workspaceSnapshotStore) CoverSnapshotSystem(ctx context.Context, orgID, taskID, claimID, fingerprint string, position float64) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE workspace_snapshots
		SET covered_position = ?, covered_fingerprint = ?
		WHERE org_id = ? AND task_id = ? AND writer_claim_id = ? AND state = 'written'
	`, position, fingerprint, orgID, taskID, claimID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *workspaceSnapshotStore) GetSnapshotStateSystem(ctx context.Context, orgID, taskID string) (*domain.WorkspaceSnapshotState, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	var st domain.WorkspaceSnapshotState
	var coveredFingerprint sql.NullString
	err := s.q.QueryRowContext(ctx, `
		SELECT org_id, task_id, state, writer_claim_id, updated_at,
		       covered_position, covered_fingerprint
		FROM workspace_snapshots
		WHERE org_id = ? AND task_id = ?
	`, orgID, taskID).Scan(&st.OrgID, &st.TaskID, &st.State, &st.WriterClaimID, &st.UpdatedAt,
		&st.CoveredPosition, &coveredFingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	st.CoveredFingerprint = coveredFingerprint.String
	return &st, nil
}

func (s *workspaceSnapshotStore) DeleteSnapshotStateSystem(ctx context.Context, orgID, taskID string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		DELETE FROM workspace_snapshots WHERE org_id = ? AND task_id = ?
	`, orgID, taskID)
	return err
}
