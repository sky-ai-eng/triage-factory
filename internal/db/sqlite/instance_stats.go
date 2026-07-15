package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// instanceStatStore is the SQLite impl of db.InstanceStatStore — the 1-minute
// fleet telemetry samples. N=1: the single process samples itself.
type instanceStatStore struct{ q queryer }

func newInstanceStatStore(q queryer) db.InstanceStatStore {
	return &instanceStatStore{q: q}
}

// NewInstanceStatStore builds a standalone store against a pooled *sql.DB,
// mirroring postgres.NewInstanceStatStore — the narrow constructor the sampler
// and CLI use without building the full db.Stores bundle.
func NewInstanceStatStore(conn *sql.DB) db.InstanceStatStore {
	return newInstanceStatStore(conn)
}

var _ db.InstanceStatStore = (*instanceStatStore)(nil)

func (s *instanceStatStore) Record(ctx context.Context, stat domain.InstanceStat) error {
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO instance_stats
			(instance_id, at, active_runs, queued_visible, mem_available_mb, cpu_pct, load1, claims, spawn_p50_ms, oom_kills)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(instance_id, at) DO NOTHING
	`,
		stat.InstanceID, stat.At.UTC(),
		sqliteNullInt(stat.ActiveRuns), sqliteNullInt(stat.QueuedVisible),
		sqliteNullInt(stat.MemAvailableMB),
		sqliteNullFloat(stat.CPUPct), sqliteNullFloat(stat.Load1),
		sqliteNullInt(stat.Claims), sqliteNullInt(stat.SpawnP50MS), sqliteNullInt(stat.OOMKills),
	)
	return err
}

func (s *instanceStatStore) ListSince(ctx context.Context, since time.Time) ([]domain.InstanceStat, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT instance_id, at, active_runs, queued_visible, mem_available_mb, cpu_pct, load1, claims, spawn_p50_ms, oom_kills
		FROM instance_stats WHERE at >= ? ORDER BY at ASC
	`, since.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanInstanceStats(rows)
}

func (s *instanceStatStore) Reap(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.q.ExecContext(ctx, `DELETE FROM instance_stats WHERE at < ?`, cutoff.UTC())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func scanInstanceStats(rows *sql.Rows) ([]domain.InstanceStat, error) {
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

func sqliteNullFloat(p *float64) sql.NullFloat64 {
	if p == nil {
		return sql.NullFloat64{}
	}
	return sql.NullFloat64{Float64: *p, Valid: true}
}

func floatPtrFromNull(n sql.NullFloat64) *float64 {
	if !n.Valid {
		return nil
	}
	v := n.Float64
	return &v
}
