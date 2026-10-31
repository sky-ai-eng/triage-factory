package host

import (
	"context"
	"time"
)

// ReadSample and OOMKills are point-in-time host telemetry for the fleet
// dashboard's 1-minute instance_stats samples: whole-host CPU utilization,
// the 1-minute load average, and this cgroup's cumulative OOM-kill count.
//
// The CPU and load readers are promoted from
// cmd/sandbox-bench/hostmetrics_linux.go, but unlike the bench the sampler
// pairs them with MemAvailableMB for memory, never a host-wide /proc/meminfo
// read, so every figure reports instance truth. On a non-Linux host, or when
// /proc or the cgroup files can't be read, the readers return nil or Unknown
// and the sampler writes SQL NULL for that column.

// Sample is a point-in-time host telemetry reading. Fields are pointers so a
// platform that can't report one leaves it nil (→ SQL NULL), distinct from a
// real zero.
type Sample struct {
	CPUPct *float64 // whole-host CPU utilization % over the sampling window
	Load1  *float64 // 1-minute load average
}

// ReadSample samples whole-host CPU utilization over `window` (it blocks for
// that long, diffing /proc/stat) and the 1-minute load average. A cancelled
// ctx cuts the window short and yields whatever could be read. Non-Linux or
// an unreadable /proc leaves the corresponding field nil.
func ReadSample(ctx context.Context, window time.Duration) Sample { return readSample(ctx, window) }

// OOMKills returns this process cgroup's cumulative oom_kill counter (cgroup
// v2 memory.events), or Unknown. Callers diff successive readings for a
// per-interval delta; a cumulative counter is what the kernel exposes.
func OOMKills() int { return oomKills() }
