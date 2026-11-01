package sysinfo

// MemAvailableMB returns available memory in MiB — container-scoped under a
// cgroup v2 memory limit, host-wide MemAvailable otherwise — or Unknown. It
// and MemTotalMB feed capacity decisions: the dispatch memory guardrail and
// the boot-time capacity log.
//
// On Linux both are CONTAINER-scoped, not host-scoped, whenever this process
// is confined by a real cgroup v2 memory limit anywhere in its own cgroup
// ancestry (walked via /proc/self/cgroup, not assumed to be /sys/fs/cgroup's
// literal mount root — a shared/host cgroup namespace resolves the same as
// the default private one). Both figures reflect the most restrictive
// ancestor level, matching how the kernel actually enforces cgroup v2's
// hierarchical memory.max. In that shape they can be far below the physical
// host's real RAM (the whole point: a `--memory=8g` container on a 64 GB host
// reports ~8 GB, not 64).
//
// They fall back to /proc/meminfo — always host-wide — only when truly
// unconfined: no cgroup, or memory.max is "max" at every level.
func MemAvailableMB() int { return memAvailableMB() }

// MemTotalMB returns total memory in MiB — container-scoped under a cgroup v2
// memory limit, host-wide MemTotal otherwise — or Unknown. MemAvailableMB's
// doc says which applies.
func MemTotalMB() int { return memTotalMB() }
