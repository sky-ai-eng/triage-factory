package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// workspaceSnapshotStore is the Postgres impl of db.WorkspaceSnapshotStore —
// the per-snapshot-key lifecycle record. Admin-pool only: workspace_snapshots
// carries no app-pool grant at all (see the table's RLS comment in the baseline
// migration), and every caller is an executor teardown goroutine or a system
// sweep with no JWT claims. org_id is bound by argument on every statement.
type workspaceSnapshotStore struct{ admin queryer }

func newWorkspaceSnapshotStore(admin queryer) db.WorkspaceSnapshotStore {
	return &workspaceSnapshotStore{admin: admin}
}

var _ db.WorkspaceSnapshotStore = (*workspaceSnapshotStore)(nil)

func (s *workspaceSnapshotStore) BeginSnapshotSystem(ctx context.Context, orgID, blueprintRunID, claimID string) error {
	_, err := s.admin.ExecContext(ctx, `
		INSERT INTO workspace_snapshots (org_id, blueprint_run_id, state, writer_claim_id, updated_at)
		VALUES ($1::uuid, $2::uuid, 'pending', $3::uuid, now())
		ON CONFLICT (org_id, blueprint_run_id) DO UPDATE SET
			state           = EXCLUDED.state,
			writer_claim_id = EXCLUDED.writer_claim_id,
			updated_at      = EXCLUDED.updated_at
	`, orgID, blueprintRunID, claimID)
	return err
}

func (s *workspaceSnapshotStore) FinishSnapshotSystem(ctx context.Context, orgID, blueprintRunID, claimID string, ok bool) (bool, error) {
	state := domain.WorkspaceSnapshotFailed
	if ok {
		state = domain.WorkspaceSnapshotWritten
	}
	res, err := s.admin.ExecContext(ctx, `
		UPDATE workspace_snapshots
		SET state = $4, updated_at = now()
		WHERE org_id = $1::uuid AND blueprint_run_id = $2::uuid
		  AND writer_claim_id = $3::uuid AND state = 'pending'
	`, orgID, blueprintRunID, claimID, state)
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
	var st domain.WorkspaceSnapshotState
	err := s.admin.QueryRowContext(ctx, `
		SELECT org_id::text, blueprint_run_id::text, state, writer_claim_id::text, updated_at
		FROM workspace_snapshots
		WHERE org_id = $1::uuid AND blueprint_run_id = $2::uuid
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
	_, err := s.admin.ExecContext(ctx, `
		DELETE FROM workspace_snapshots WHERE org_id = $1::uuid AND blueprint_run_id = $2::uuid
	`, orgID, blueprintRunID)
	return err
}
