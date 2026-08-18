package domain

import "time"

// Instance is one row in the instances table — the fleet membership
// registry every TF process registers into at boot and refreshes via
// periodic heartbeat. Role-neutral by design: control pods register here
// too (for deployment-wide visibility — versions, lease holder, health),
// not just executors; the capacity/admission fields are meaningful only
// for executor-capable roles and stay nil on pure-control rows.
type Instance struct {
	ID              string
	Role            string
	Version         string
	BootEpoch       int64
	StartedAt       time.Time
	LastHeartbeatAt time.Time
	Draining        bool

	// Capacity + admission snapshot, refreshed on every heartbeat.
	// Executor-capable roles only; nil on pure-control rows.
	MaxRuns        *int
	ActiveRuns     *int
	MemTotalMB     *int
	MemAvailableMB *int
	DispatchGated  *bool

	// LabelsJSON is a raw JSON object (future: sandbox-fleet profile
	// classes for placement). Empty string when unset.
	LabelsJSON string

	// PubKey is this boot's ephemeral X25519 public key (base64), minted
	// in-memory at process start and never persisted — a restart mints a
	// fresh one (TFAC-614). Written only by Register, never the
	// heartbeat; empty on a control/all row that never claims conversations.
	PubKey string
}

// Instance roles — the value app.registerInstance stamps here from
// runmode.Role() (TFAC-582). "all" is the local-only single-process shape
// (multi rejects it at boot); "control" and "executor" are the two halves of
// the split every multi deployment runs. These string values MUST match
// runmode's DeployRole constants (RoleAll / RoleControl / RoleExecutor) —
// registration passes string(runmode.Role()) straight through.
const (
	InstanceRoleAll      = "all"
	InstanceRoleControl  = "control"
	InstanceRoleExecutor = "executor"
)

// InstanceHeartbeat carries the periodic capacity + admission snapshot a
// heartbeat renewal writes onto the caller's instances row. Pointer fields
// left nil write SQL NULL — the "not applicable / not yet known" state, not
// zero.
//
// Deliberately absent: draining and labels. Those columns hold operator /
// control-plane intent (a drain verb, placement labels), and the heartbeat
// must never overwrite them — a 4s liveness loop that resets draining=false
// would silently cancel a drain within one tick of the operator setting it.
// The heartbeat owns liveness + capacity; intent columns are written only
// by the surfaces that own them.
type InstanceHeartbeat struct {
	MaxRuns        *int
	ActiveRuns     *int
	MemTotalMB     *int
	MemAvailableMB *int
	DispatchGated  *bool
}
