package sqlite

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// sandboxStatStore is the SQLite impl of db.SandboxStatStore — the
// per-sandbox resource series. No production caller reaches this arm: local
// mode never sandboxes, so there is no jail to sample and the sampler's batch
// is always empty. It exists because the store is one dual-dialect contract
// with one conformance suite, the same reason claims' measured-actuals arm
// does.
type sandboxStatStore struct{ q queryer }

func newSandboxStatStore(q queryer) db.SandboxStatStore {
	return &sandboxStatStore{q: q}
}

// NewSandboxStatStore builds a standalone store against a pooled *sql.DB,
// mirroring postgres.NewSandboxStatStore — the narrow constructor a caller
// uses without building the full db.Stores bundle.
func NewSandboxStatStore(conn *sql.DB) db.SandboxStatStore {
	return newSandboxStatStore(conn)
}

var _ db.SandboxStatStore = (*sandboxStatStore)(nil)

func (s *sandboxStatStore) RecordBatch(ctx context.Context, stats []domain.SandboxStat) error {
	if len(stats) == 0 {
		return nil
	}
	// One multi-row VALUES insert: a tick produces a row per live jail, and
	// the whole tick is one round trip.
	var sb strings.Builder
	sb.WriteString(`INSERT INTO sandbox_stats (claim_id, at, mem_current_mb, cpu_usec_cum) VALUES `)
	args := make([]any, 0, len(stats)*4)
	for i, st := range stats {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString("(?, ?, ?, ?)")
		args = append(args, st.ClaimID, st.At.UTC(), sqliteNullInt(st.MemCurrentMB), sqliteNullInt64(st.CPUUsecCum))
	}
	sb.WriteString(` ON CONFLICT(claim_id, at) DO NOTHING`)
	_, err := s.q.ExecContext(ctx, sb.String(), args...)
	return err
}

func (s *sandboxStatStore) ListForClaim(ctx context.Context, claimID string) ([]domain.SandboxStat, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT claim_id, at, mem_current_mb, cpu_usec_cum
		FROM sandbox_stats WHERE claim_id = ? ORDER BY at ASC
	`, claimID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSandboxStats(rows)
}

func (s *sandboxStatStore) Reap(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.q.ExecContext(ctx, `DELETE FROM sandbox_stats WHERE at < ?`, cutoff.UTC())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func scanSandboxStats(rows *sql.Rows) ([]domain.SandboxStat, error) {
	var out []domain.SandboxStat
	for rows.Next() {
		var st domain.SandboxStat
		var mem, cpu sql.NullInt64
		if err := rows.Scan(&st.ClaimID, &st.At, &mem, &cpu); err != nil {
			return nil, err
		}
		st.MemCurrentMB = intPtrFromNull(mem)
		st.CPUUsecCum = int64PtrFromNull(cpu)
		out = append(out, st)
	}
	return out, rows.Err()
}

// int64PtrFromNull is the 64-bit sibling of intPtrFromNull: NULL scans to nil
// so an unobserved metric stays distinguishable from a measured zero.
func int64PtrFromNull(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}
