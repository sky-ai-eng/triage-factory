// Package sysinfo reads what the kernel reports about the system this
// process runs in: memory and disk for capacity decisions, CPU, load and OOM
// kills for the fleet dashboard's instance samples, and the time the system
// has spent suspended, for leases stamped on database time. That is not
// always the physical machine: memory and OOM kills are the container's
// cgroup when one confines this process, while CPU and load are whole-host.
//
// Every reader is best-effort and fails soft. Where a platform or host
// cannot report a figure, the reader returns Unknown (or leaves a field nil)
// instead of an error, because a probe failure must never wedge dispatch or
// telemetry.
package sysinfo

// Unknown is the reading of a figure this platform or host cannot report
// (non-Linux, an unreadable /proc or cgroup file, no path statfs answers
// for). Capacity guardrails treat it as "no gating", so they fail open, and
// the instance sampler writes it as SQL NULL.
const Unknown = -1
