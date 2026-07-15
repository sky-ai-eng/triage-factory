package postgres

import (
	"context"
	"database/sql"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// instanceStatStore is the Postgres impl of db.InstanceStatStore — the
// 1-minute fleet telemetry samples. Wired against the ADMIN (BYPASSRLS) pool,
// same posture as instances: instance_stats is a system-only table (RLS
// deny-by-default, REVOKEd from the app roles), so the superuser/tf_system
// pool does all I/O with no org to scope on.
type instanceStatStore struct{ admin queryer }

func newInstanceStatStore(admin queryer) db.InstanceStatStore {
	return &instanceStatStore{admin: admin}
}

// NewInstanceStatStore builds a standalone store against a pooled *sql.DB,
// mirroring postgres.NewInstanceStore — the narrow constructor the sampler
// uses without building the full db.Stores bundle.
func NewInstanceStatStore(admin *sql.DB) db.InstanceStatStore {
	return newInstanceStatStore(admin)
}

var _ db.InstanceStatStore = (*instanceStatStore)(nil)

func (s *instanceStatStore) Record(ctx context.Context, stat domain.InstanceStat) error {
	_, err := s.admin.ExecContext(ctx, `
		INSERT INTO instance_stats
			(instance_id, at, active_runs, queued_visible, mem_available_mb, cpu_pct, load1, claims, spawn_p50_ms, oom_kills)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (instance_id, at) DO NOTHING
	`,
		stat.InstanceID, stat.At.UTC(),
		intPtrAny(stat.ActiveRuns), intPtrAny(stat.QueuedVisible),
		intPtrAny(stat.MemAvailableMB),
		floatPtrAny(stat.CPUPct), floatPtrAny(stat.Load1),
		intPtrAny(stat.Claims), intPtrAny(stat.SpawnP50MS), intPtrAny(stat.OOMKills),
	)
	if err != nil {
		return wrapAdminPoolPermErr(err, "instance_stats.Record")
	}
	return nil
}

func (s *instanceStatStore) ListSince(ctx context.Context, since time.Time) ([]domain.InstanceStat, error) {
	rows, err := s.admin.QueryContext(ctx, `
		SELECT instance_id, at, active_runs, queued_visible, mem_available_mb, cpu_pct, load1, claims, spawn_p50_ms, oom_kills
		FROM instance_stats WHERE at >= $1 ORDER BY at ASC
	`, since.UTC())
	if err != nil {
		return nil, wrapAdminPoolPermErr(err, "instance_stats.ListSince")
	}
	defer rows.Close()

	var out []domain.InstanceStat
	for rows.Next() {
		var st domain.InstanceStat
		var active, queued, mem, claims, spawn, oom sql.NullInt64
		var cpu, load sql.NullFloat64
		if err := rows.Scan(
			&st.InstanceID, &st.At, &active, &queued, &mem, &cpu, &load, &claims, &spawn, &oom,
		); err != nil {
			return nil, err
		}
		st.ActiveRuns = intPtrFromNull(active)
		st.QueuedVisible = intPtrFromNull(queued)
		st.MemAvailableMB = intPtrFromNull(mem)
		st.CPUPct = floatPtrFromNull(cpu)
		st.Load1 = floatPtrFromNull(load)
		st.Claims = intPtrFromNull(claims)
		st.SpawnP50MS = intPtrFromNull(spawn)
		st.OOMKills = intPtrFromNull(oom)
		out = append(out, st)
	}
	return out, rows.Err()
}

func (s *instanceStatStore) Reap(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.admin.ExecContext(ctx, `DELETE FROM instance_stats WHERE at < $1`, cutoff.UTC())
	if err != nil {
		return 0, wrapAdminPoolPermErr(err, "instance_stats.Reap")
	}
	return res.RowsAffected()
}

// floatPtrAny maps a *float64 to a bind-compatible value (nil for NULL) —
// the float sibling of intPtrAny.
func floatPtrAny(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

// floatPtrFromNull is the read-side counterpart: NULL scans to nil.
func floatPtrFromNull(n sql.NullFloat64) *float64 {
	if !n.Valid {
		return nil
	}
	v := n.Float64
	return &v
}
