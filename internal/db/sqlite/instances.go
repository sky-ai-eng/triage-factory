package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// instanceStore is the SQLite impl of db.InstanceStore — the fleet
// membership registry. SQLite is N=1: one process, one row, boot_epoch
// bumping per restart. No RLS concept, so every method runs against the
// single connection.
type instanceStore struct{ q queryer }

func newInstanceStore(q queryer) db.InstanceStore {
	return &instanceStore{q: q}
}

// NewInstanceStore builds a standalone db.InstanceStore against a pooled
// *sql.DB — mirrors postgres.NewInstanceStore; the narrow constructor CLI
// subcommands use (TFAC-586) instead of building the full db.Stores bundle
// just to reach .Instances.
func NewInstanceStore(conn *sql.DB) db.InstanceStore {
	return newInstanceStore(conn)
}

var _ db.InstanceStore = (*instanceStore)(nil)

func (s *instanceStore) Register(ctx context.Context, id, role, version string) (int64, error) {
	now := time.Now().UTC()
	var bootEpoch int64
	err := s.q.QueryRowContext(ctx, `
		INSERT INTO instances (id, role, version, boot_epoch, started_at, last_heartbeat_at)
		VALUES (?, ?, ?, 1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			boot_epoch        = instances.boot_epoch + 1,
			role              = excluded.role,
			version           = excluded.version,
			started_at        = excluded.started_at,
			last_heartbeat_at = excluded.last_heartbeat_at,
			-- Clear the prior boot's capacity/admission snapshot (dead
			-- state); the first heartbeat repopulates it. draining and
			-- labels survive on purpose — operator intent outlives a
			-- restart. Mirrors the Postgres store.
			max_runs          = NULL,
			active_runs       = NULL,
			mem_total_mb      = NULL,
			mem_available_mb  = NULL,
			dispatch_gated    = NULL
		RETURNING boot_epoch
	`, id, role, version, now, now).Scan(&bootEpoch)
	return bootEpoch, err
}

func (s *instanceStore) Heartbeat(ctx context.Context, id string, bootEpoch int64, hb domain.InstanceHeartbeat) (bool, bool, error) {
	// draining and labels_json are deliberately not in the SET list — they
	// hold operator/control-plane intent and the heartbeat must not clobber
	// them (see domain.InstanceHeartbeat). draining IS read back via
	// RETURNING (SQLite supports it, same as Register above and every
	// other claim/UPDATE in this package) so the read is atomic with the
	// (id, boot_epoch)-fenced UPDATE — a follow-up SELECT filtered on id
	// alone would race a concurrent re-register of the same id (a second
	// process/goroutine bumping boot_epoch between the UPDATE and the
	// SELECT), reading the NEWER boot's draining value under the OLDER
	// boot's identity.
	var draining bool
	err := s.q.QueryRowContext(ctx, `
		UPDATE instances SET
			last_heartbeat_at = ?,
			max_runs          = ?,
			active_runs       = ?,
			mem_total_mb      = ?,
			mem_available_mb  = ?,
			dispatch_gated    = ?
		WHERE id = ? AND boot_epoch = ?
		RETURNING draining
	`,
		time.Now().UTC(),
		sqliteNullInt(hb.MaxRuns), sqliteNullInt(hb.ActiveRuns),
		sqliteNullInt(hb.MemTotalMB), sqliteNullInt(hb.MemAvailableMB),
		sqliteNullBool(hb.DispatchGated),
		id, bootEpoch,
	).Scan(&draining)
	if err == sql.ErrNoRows {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return true, draining, nil
}

func (s *instanceStore) List(ctx context.Context) ([]domain.Instance, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT id, role, version, boot_epoch, started_at, last_heartbeat_at, draining,
		       max_runs, active_runs, mem_total_mb, mem_available_mb, dispatch_gated, labels_json
		FROM instances ORDER BY started_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.Instance
	for rows.Next() {
		var inst domain.Instance
		var maxRuns, activeRuns, memTotal, memAvail sql.NullInt64
		var dispatchGated sql.NullBool
		var labels sql.NullString
		if err := rows.Scan(
			&inst.ID, &inst.Role, &inst.Version, &inst.BootEpoch, &inst.StartedAt, &inst.LastHeartbeatAt, &inst.Draining,
			&maxRuns, &activeRuns, &memTotal, &memAvail, &dispatchGated, &labels,
		); err != nil {
			return nil, err
		}
		inst.MaxRuns = intPtrFromNull(maxRuns)
		inst.ActiveRuns = intPtrFromNull(activeRuns)
		inst.MemTotalMB = intPtrFromNull(memTotal)
		inst.MemAvailableMB = intPtrFromNull(memAvail)
		if dispatchGated.Valid {
			v := dispatchGated.Bool
			inst.DispatchGated = &v
		}
		inst.LabelsJSON = labels.String
		out = append(out, inst)
	}
	return out, rows.Err()
}

func (s *instanceStore) SetDraining(ctx context.Context, id string, draining bool) (bool, error) {
	res, err := s.q.ExecContext(ctx, `UPDATE instances SET draining = ? WHERE id = ?`, draining, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *instanceStore) Get(ctx context.Context, id string) (*domain.Instance, error) {
	var inst domain.Instance
	var maxRuns, activeRuns, memTotal, memAvail sql.NullInt64
	var dispatchGated sql.NullBool
	var labels sql.NullString

	err := s.q.QueryRowContext(ctx, `
		SELECT id, role, version, boot_epoch, started_at, last_heartbeat_at, draining,
		       max_runs, active_runs, mem_total_mb, mem_available_mb, dispatch_gated, labels_json
		FROM instances WHERE id = ?
	`, id).Scan(
		&inst.ID, &inst.Role, &inst.Version, &inst.BootEpoch, &inst.StartedAt, &inst.LastHeartbeatAt, &inst.Draining,
		&maxRuns, &activeRuns, &memTotal, &memAvail, &dispatchGated, &labels,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	inst.MaxRuns = intPtrFromNull(maxRuns)
	inst.ActiveRuns = intPtrFromNull(activeRuns)
	inst.MemTotalMB = intPtrFromNull(memTotal)
	inst.MemAvailableMB = intPtrFromNull(memAvail)
	if dispatchGated.Valid {
		v := dispatchGated.Bool
		inst.DispatchGated = &v
	}
	inst.LabelsJSON = labels.String
	return &inst, nil
}

// --- Helpers ---

// sqliteNullBool mirrors sqliteNullInt (agentrun.go) for the one *bool
// heartbeat field.
func sqliteNullBool(p *bool) sql.NullBool {
	if p == nil {
		return sql.NullBool{}
	}
	return sql.NullBool{Bool: *p, Valid: true}
}

func intPtrFromNull(n sql.NullInt64) *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Int64)
	return &v
}
