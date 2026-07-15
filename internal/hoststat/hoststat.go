// Package hoststat reads point-in-time host telemetry for the fleet
// dashboard's 1-minute instance_stats samples — whole-host CPU utilization,
// the 1-minute load average, and this cgroup's cumulative OOM-kill count.
//
// It is the sibling of internal/hostmem (which owns the memory figures the
// dispatch guardrail reads): the CPU/load readers here are promoted from
// cmd/sandbox-bench/hostmetrics_linux.go, but — unlike the bench — the sampler
// pairs them with hostmem for memory, never a host-wide /proc/meminfo read, so
// every figure reports instance truth.
//
// Everything is best-effort and fails soft: on a non-Linux host, or when /proc
// or the cgroup files can't be read, the readers return nil / Unknown and the
// sampler simply writes SQL NULL for that column. A telemetry read must never
// wedge anything.
package hoststat

import (
	"context"
	"time"
)

// Unknown is returned by OOMKills when the count can't be read (non-Linux, no
// cgroup v2 memory controller, unreadable file).
const Unknown = -1

// Sample is a point-in-time host telemetry reading. Fields are pointers so a
// platform that can't report one leaves it nil (→ SQL NULL), distinct from a
// real zero.
type Sample struct {
	CPUPct *float64 // whole-host CPU utilization % over the sampling window
	Load1  *float64 // 1-minute load average
}

// Read samples whole-host CPU utilization over `window` (it blocks for that
// long, diffing /proc/stat) and the 1-minute load average. A cancelled ctx
// cuts the window short and yields whatever could be read. Non-Linux or an
// unreadable /proc leaves the corresponding field nil.
func Read(ctx context.Context, window time.Duration) Sample { return read(ctx, window) }

// OOMKills returns this process cgroup's cumulative oom_kill counter (cgroup
// v2 memory.events), or Unknown. Callers diff successive readings for a
// per-interval delta; a cumulative counter is what the kernel exposes.
func OOMKills() int { return oomKills() }
