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
}

// Instance roles. "all" is the only role that exists today — TF_ROLE (the
// control/executor split) is a later phase of the same epic — so every
// process registers as InstanceRoleAll until that split lands.
const (
	InstanceRoleAll      = "all"
	InstanceRoleControl  = "control"
	InstanceRoleExecutor = "executor"
)

// InstanceHeartbeat carries the periodic capacity + admission snapshot a
// heartbeat renewal writes onto the caller's instances row. Pointer fields
// left nil write SQL NULL — the "not applicable / not yet known" state, not
// zero.
type InstanceHeartbeat struct {
	Draining       bool
	MaxRuns        *int
	ActiveRuns     *int
	MemTotalMB     *int
	MemAvailableMB *int
	DispatchGated  *bool
	LabelsJSON     string
}
