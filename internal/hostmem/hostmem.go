// Package hostmem reads memory figures for capacity decisions — the
// dispatch memory guardrail and the boot-time capacity log. On Linux
// it prefers the cgroup v2 memory controller's view (memory.max /
// memory.current / memory.stat) over /proc/meminfo whenever this
// process is confined by a real limit, since /proc/meminfo is always
// host-wide and understates how tight things are inside a
// memory-limited container (the standard compose shape, Fly
// Machines). It falls back to /proc/meminfo when unconfined (no
// cgroup, or memory.max is "max") — the same figures this package
// always returned.
package hostmem

// Unknown is returned when the platform can't report a figure
// (non-Linux, unreadable /proc). Callers treat it as "no gating" —
// capacity guardrails fail open so a probe failure can never wedge
// dispatch.
const Unknown = -1

// AvailableMB returns the host's MemAvailable in MiB, or Unknown.
func AvailableMB() int { return availableMB() }

// TotalMB returns the host's MemTotal in MiB, or Unknown.
func TotalMB() int { return totalMB() }
