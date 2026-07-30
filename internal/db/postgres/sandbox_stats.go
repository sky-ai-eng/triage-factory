package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// sandboxStatStore is the Postgres impl of db.SandboxStatStore — the
// per-sandbox resource series. Wired against the ADMIN (BYPASSRLS) pool, same
// posture as instanceStatStore: sandbox_stats is a system-only table (RLS
// deny-by-default, REVOKEd from the app roles) written by the executor's
// sampler, which holds no request identity to scope on.
type sandboxStatStore struct{ admin queryer }

func newSandboxStatStore(admin queryer) db.SandboxStatStore {
	return &sandboxStatStore{admin: admin}
}

// NewSandboxStatStore builds a standalone store against a pooled *sql.DB,
// mirroring postgres.NewInstanceStatStore — the narrow constructor a caller
// uses without building the full db.Stores bundle.
func NewSandboxStatStore(admin *sql.DB) db.SandboxStatStore {
	return newSandboxStatStore(admin)
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
		n := i * 4
		fmt.Fprintf(&sb, "($%d, $%d, $%d, $%d)", n+1, n+2, n+3, n+4)
		args = append(args, st.ClaimID, st.At.UTC(), intPtrAny(st.MemCurrentMB), nullInt64Ptr(st.CPUUsecCum))
	}
	sb.WriteString(` ON CONFLICT (claim_id, at) DO NOTHING`)
	if _, err := s.admin.ExecContext(ctx, sb.String(), args...); err != nil {
		return wrapAdminPoolPermErr(err, "sandbox_stats.RecordBatch")
	}
	return nil
}

func (s *sandboxStatStore) ListForClaim(ctx context.Context, claimID string) ([]domain.SandboxStat, error) {
	rows, err := s.admin.QueryContext(ctx, `
		SELECT claim_id, at, mem_current_mb, cpu_usec_cum
		FROM sandbox_stats WHERE claim_id = $1 ORDER BY at ASC
	`, claimID)
	if err != nil {
		return nil, wrapAdminPoolPermErr(err, "sandbox_stats.ListForClaim")
	}
	defer rows.Close()

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

func (s *sandboxStatStore) Reap(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.admin.ExecContext(ctx, `DELETE FROM sandbox_stats WHERE at < $1`, cutoff.UTC())
	if err != nil {
		return 0, wrapAdminPoolPermErr(err, "sandbox_stats.Reap")
	}
	return res.RowsAffected()
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
