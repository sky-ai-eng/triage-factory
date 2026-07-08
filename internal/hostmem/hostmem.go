// Package hostmem reads memory figures for capacity decisions — the
// dispatch memory guardrail and the boot-time capacity log.
//
// On Linux, AvailableMB and TotalMB are CONTAINER-scoped, not
// host-scoped, whenever this process is confined by a real cgroup v2
// memory limit anywhere in its own cgroup ancestry (walked via
// /proc/self/cgroup, not assumed to be /sys/fs/cgroup's literal mount
// root — a shared/host cgroup namespace resolves the same as the
// default private one). Both figures reflect the most restrictive
// ancestor level, matching how the kernel actually enforces cgroup
// v2's hierarchical memory.max. In that shape they can be far below
// the physical host's real RAM (the whole point: a `--memory=8g`
// container on a 64 GB host reports ~8 GB, not 64).
//
// They fall back to /proc/meminfo — always host-wide — only when
// truly unconfined: no cgroup, or memory.max is "max" at every level.
package hostmem

// Unknown is returned when the platform can't report a figure
// (non-Linux, unreadable /proc). Callers treat it as "no gating" —
// capacity guardrails fail open so a probe failure can never wedge
// dispatch.
const Unknown = -1

// AvailableMB returns available memory in MiB — container-scoped
// under a cgroup v2 memory limit, host-wide MemAvailable otherwise —
// or Unknown. See the package doc for which applies.
func AvailableMB() int { return availableMB() }

// TotalMB returns total memory in MiB — container-scoped under a
// cgroup v2 memory limit, host-wide MemTotal otherwise — or Unknown.
// See the package doc for which applies.
func TotalMB() int { return totalMB() }
