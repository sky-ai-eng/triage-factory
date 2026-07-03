// Package hostmem reads host-level memory figures for capacity
// decisions — the dispatch memory guardrail and the boot-time
// capacity log. Inside the multi-mode container /proc/meminfo is the
// host-wide view (the TF service sets no cgroup memory limit), which
// is exactly what fleet-level gating wants.
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
