// Package host reads figures about the machine this process runs on from
// the kernel: memory and disk for capacity decisions, CPU, load and OOM
// kills for the fleet dashboard's instance samples, and the time the system
// has spent suspended, for leases stamped on database time.
//
// Every reader is best-effort and fails soft. Where a platform or host
// cannot report a figure, the reader returns Unknown (or leaves a field nil)
// instead of an error, because a probe failure must never wedge dispatch or
// telemetry.
package host

// Unknown is the reading of a figure this platform or host cannot report
// (non-Linux, an unreadable /proc or cgroup file, no path statfs answers
// for). Capacity guardrails treat it as "no gating", so they fail open, and
// the instance sampler writes it as SQL NULL.
const Unknown = -1
