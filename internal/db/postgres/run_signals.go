package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// runSignalStore is the Postgres impl of db.RunSignalStore — the cross-pod
// run-control outbox (TFAC-585). Admin-pool only: run_signals carries no
// app-pool policy (RLS enabled, REVOKE ALL from the app roles — see the
// migration), so every statement here runs against the admin/BYPASSRLS
// pool, mirroring InstanceStore's shape.
type runSignalStore struct{ admin queryer }

func newRunSignalStore(admin queryer) db.RunSignalStore { return &runSignalStore{admin: admin} }

var _ db.RunSignalStore = (*runSignalStore)(nil)

func (s *runSignalStore) Insert(ctx context.Context, orgID, runID string, kind domain.RunSignalKind, payload, target string) (int64, error) {
	var payloadArg any
	if payload != "" {
		payloadArg = []byte(payload)
	}
	var id int64
	err := s.admin.QueryRowContext(ctx, `
		INSERT INTO run_signals (org_id, conversation_id, kind, payload, target, created_at)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5, now())
		RETURNING id
	`, orgID, runID, string(kind), payloadArg, target).Scan(&id)
	return id, err
}

func (s *runSignalStore) ListUnackedForTarget(ctx context.Context, target string) ([]domain.RunSignal, error) {
	rows, err := s.admin.QueryContext(ctx, `
		SELECT id, org_id::text, conversation_id::text, kind, COALESCE(payload::text, ''), target, created_at
		FROM run_signals
		WHERE target = $1 AND acked_at IS NULL
		ORDER BY id
	`, target)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.RunSignal
	for rows.Next() {
		var sig domain.RunSignal
		var kind string
		if err := rows.Scan(&sig.ID, &sig.OrgID, &sig.RunID, &kind, &sig.Payload, &sig.Target, &sig.CreatedAt); err != nil {
			return nil, err
		}
		sig.Kind = domain.RunSignalKind(kind)
		out = append(out, sig)
	}
	return out, rows.Err()
}

func (s *runSignalStore) Ack(ctx context.Context, id int64, result string) error {
	_, err := s.admin.ExecContext(ctx, `
		UPDATE run_signals SET acked_at = now(), ack_result = $2
		WHERE id = $1 AND acked_at IS NULL
	`, id, result)
	return err
}

func (s *runSignalStore) AckStatus(ctx context.Context, id int64) (bool, string, error) {
	var ackedAt sql.NullTime
	var result sql.NullString
	err := s.admin.QueryRowContext(ctx, `SELECT acked_at, ack_result FROM run_signals WHERE id = $1`, id).Scan(&ackedAt, &result)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	return ackedAt.Valid, result.String, nil
}

func (s *runSignalStore) PurgeAcked(ctx context.Context, olderThan time.Duration) (int, error) {
	res, err := s.admin.ExecContext(ctx, `
		DELETE FROM run_signals WHERE acked_at IS NOT NULL AND acked_at < $1
	`, time.Now().Add(-olderThan))
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}
