package domain

import "time"

// SandboxStat is one mid-engagement observation of a single sandbox's live resource
// use, sampled from its cgroup while the jail is still running and keyed to
// the claim that pays for it.
//
// This is the SHAPE of an engagement's consumption over time — the thing the claim's
// teardown scalars (peak memory, cumulative CPU at exit) structurally cannot
// give. The two disagree slightly by design: a periodic sampler misses the
// sub-minute spikes the kernel's high-watermark catches, so the series is
// shape and the teardown snapshot is truth. They are never reconciled
// numerically.
//
// CPU is CUMULATIVE, not a rate. A consumer derives rate from the difference
// between two samples, which means a dropped tick self-heals into a
// wider-but-correct interval instead of leaving a gap that reads as idle.
//
// Both metrics are pointers for the same reason InstanceStat's are: a partial
// sample is still worth keeping, and nil writes SQL NULL — "not observed" —
// as distinct from a measured zero.
type SandboxStat struct {
	// ClaimID is the executor engagement whose jail was sampled. The series
	// joins to the claim's end-state actuals through this key.
	ClaimID string
	// At is the sampling instant. Every sandbox sampled on one tick shares
	// it, so a fleet-wide read lines the series up without bucketing.
	At time.Time

	// MemCurrentMB is the jail's resident memory at the sampling instant
	// (cgroup memory.current), rounded up to MiB.
	MemCurrentMB *int
	// CPUUsecCum is the jail's cumulative CPU time in microseconds
	// (cgroup cpu.stat usage_usec), user + system across the whole sandbox
	// tree. Monotonically non-decreasing for a given claim.
	CPUUsecCum *int64
}
