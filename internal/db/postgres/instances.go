package postgres

import (
	"context"
	"database/sql"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// instanceStore is the Postgres impl of db.InstanceStore — the fleet
// membership registry. Wired against the ADMIN (BYPASSRLS) pool in
// postgres.New: instances is a system-only table (RLS deny-by-default,
// REVOKEd from the app roles, exactly like public.auth_events) — a fleet
// member isn't tenant data, so there's no org to scope an app-pool policy
// on. The superuser pool does all I/O.
type instanceStore struct{ admin queryer }

func newInstanceStore(admin queryer) db.InstanceStore {
	return &instanceStore{admin: admin}
}

// NewInstanceStore builds a standalone db.InstanceStore against a pooled
// *sql.DB — the narrow constructor CLI subcommands use (`triagefactory
// instance list/drain/undrain`, TFAC-586) instead of building the full
// db.Stores bundle (New) just to reach .Instances.
func NewInstanceStore(admin *sql.DB) db.InstanceStore {
	return newInstanceStore(admin)
}

var _ db.InstanceStore = (*instanceStore)(nil)

func (s *instanceStore) Register(ctx context.Context, id, role, version string) (int64, error) {
	var bootEpoch int64
	err := s.admin.QueryRowContext(ctx, `
		INSERT INTO instances (id, role, version, boot_epoch, started_at, last_heartbeat_at)
		VALUES ($1, $2, $3, 1, now(), now())
		ON CONFLICT (id) DO UPDATE SET
			boot_epoch        = instances.boot_epoch + 1,
			role              = EXCLUDED.role,
			version           = EXCLUDED.version,
			started_at        = EXCLUDED.started_at,
			last_heartbeat_at = EXCLUDED.last_heartbeat_at,
			-- The prior boot's capacity/admission snapshot is dead state
			-- (e.g. a dispatch_gated=true from the crashed life); clear it
			-- so readers never see a new epoch wearing old admission data.
			-- The first heartbeat repopulates within one interval. draining
			-- and labels survive on purpose — operator intent outlives a
			-- restart (a drained instance stays drained until un-drained).
			max_runs          = NULL,
			active_runs       = NULL,
			mem_total_mb      = NULL,
			mem_available_mb  = NULL,
			dispatch_gated    = NULL
		RETURNING boot_epoch
	`, id, role, version).Scan(&bootEpoch)
	return bootEpoch, err
}

func (s *instanceStore) Heartbeat(ctx context.Context, id string, bootEpoch int64, hb domain.InstanceHeartbeat) (bool, bool, error) {
	// draining and labels are deliberately not in the SET list — they hold
	// operator/control-plane intent and the heartbeat must not clobber them
	// (see domain.InstanceHeartbeat). draining IS read back via RETURNING
	// so a running instance learns an operator drained it on its very
	// next heartbeat, with no separate poll (TFAC-586).
	var draining bool
	err := s.admin.QueryRowContext(ctx, `
		UPDATE instances SET
			last_heartbeat_at = now(),
			max_runs          = $3,
			active_runs       = $4,
			mem_total_mb      = $5,
			mem_available_mb  = $6,
			dispatch_gated    = $7
		WHERE id = $1 AND boot_epoch = $2
		RETURNING draining
	`,
		id, bootEpoch,
		intPtrAny(hb.MaxRuns), intPtrAny(hb.ActiveRuns),
		intPtrAny(hb.MemTotalMB), intPtrAny(hb.MemAvailableMB),
		boolPtrAny(hb.DispatchGated),
	).Scan(&draining)
	if err == sql.ErrNoRows {
		return false, false, nil
	}
	if err != nil {
		return false, false, wrapAdminPoolPermErr(err, "instances.Heartbeat")
	}
	return true, draining, nil
}

func (s *instanceStore) List(ctx context.Context) ([]domain.Instance, error) {
	rows, err := s.admin.QueryContext(ctx, `
		SELECT id, role, version, boot_epoch, started_at, last_heartbeat_at, draining,
		       max_runs, active_runs, mem_total_mb, mem_available_mb, dispatch_gated,
		       COALESCE(labels::text, '')
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
		var labels string
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
		inst.LabelsJSON = labels
		out = append(out, inst)
	}
	return out, rows.Err()
}

func (s *instanceStore) SetDraining(ctx context.Context, id string, draining bool) (bool, error) {
	res, err := s.admin.ExecContext(ctx, `UPDATE instances SET draining = $2 WHERE id = $1`, id, draining)
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
	var labels string

	err := s.admin.QueryRowContext(ctx, `
		SELECT id, role, version, boot_epoch, started_at, last_heartbeat_at, draining,
		       max_runs, active_runs, mem_total_mb, mem_available_mb, dispatch_gated,
		       COALESCE(labels::text, '')
		FROM instances WHERE id = $1
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
	inst.LabelsJSON = labels
	return &inst, nil
}

// --- Helpers ---

// boolPtrAny maps a *bool to a bind-compatible value (nil for NULL, bool
// otherwise). Postgres-side sibling of intPtrAny (curator.go).
func boolPtrAny(p *bool) any {
	if p == nil {
		return nil
	}
	return *p
}

// intPtrFromNull is the read-side counterpart to intPtrAny: NULL scans to
// nil rather than a spurious 0.
func intPtrFromNull(n sql.NullInt64) *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Int64)
	return &v
}
