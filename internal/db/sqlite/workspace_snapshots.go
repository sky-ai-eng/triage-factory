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

func (s *workspaceSnapshotStore) BeginSnapshotSystem(ctx context.Context, orgID, blueprintRunID, claimID string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO workspace_snapshots (org_id, blueprint_run_id, state, writer_claim_id, updated_at)
		VALUES (?, ?, 'pending', ?, ?)
		ON CONFLICT(org_id, blueprint_run_id) DO UPDATE SET
			state           = excluded.state,
			writer_claim_id = excluded.writer_claim_id,
			updated_at      = excluded.updated_at
	`, orgID, blueprintRunID, claimID, time.Now().UTC())
	return err
}

func (s *workspaceSnapshotStore) FinishSnapshotSystem(ctx context.Context, orgID, blueprintRunID, claimID string, ok bool) (bool, error) {
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
		WHERE org_id = ? AND blueprint_run_id = ? AND writer_claim_id = ? AND state = 'pending'
	`, state, time.Now().UTC(), orgID, blueprintRunID, claimID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *workspaceSnapshotStore) GetSnapshotStateSystem(ctx context.Context, orgID, blueprintRunID string) (*domain.WorkspaceSnapshotState, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	var st domain.WorkspaceSnapshotState
	err := s.q.QueryRowContext(ctx, `
		SELECT org_id, blueprint_run_id, state, writer_claim_id, updated_at
		FROM workspace_snapshots
		WHERE org_id = ? AND blueprint_run_id = ?
	`, orgID, blueprintRunID).Scan(&st.OrgID, &st.BlueprintRunID, &st.State, &st.WriterClaimID, &st.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &st, nil
}

func (s *workspaceSnapshotStore) DeleteSnapshotStateSystem(ctx context.Context, orgID, blueprintRunID string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		DELETE FROM workspace_snapshots WHERE org_id = ? AND blueprint_run_id = ?
	`, orgID, blueprintRunID)
	return err
}
