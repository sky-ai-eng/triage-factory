package domain

import "time"

// InstanceStat is one row in the instance_stats table — a 1-minute telemetry
// sample written by each pod's sampler. The instances registry row carries the
// *current* capacity snapshot; this carries its *history* plus the fields too
// transient for the 4s heartbeat to record usefully (per-sample claim rate,
// spawn latency, OOM kills). It backs the fleet dashboard's timeseries and the
// wait/utilization windows.
//
// Every metric is a pointer because a partial sample is still worth keeping: a
// control pod has no runs (ActiveRuns/QueuedVisible/Claims/SpawnP50MS stay
// nil), and a non-Linux or unconfined host can't read cpu/load/oom. A nil
// field writes SQL NULL — "not applicable / not observed", not zero.
// MemAvailableMB is cgroup-aware instance truth (hostmem), never a host-wide
// /proc/meminfo figure.
type InstanceStat struct {
	InstanceID string
	At         time.Time

	ActiveRuns     *int
	QueuedVisible  *int
	MemAvailableMB *int
	CPUPct         *float64
	Load1          *float64
	Claims         *int
	SpawnP50MS     *int
	OOMKills       *int
}
