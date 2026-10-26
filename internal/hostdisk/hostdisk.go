// Package hostdisk reads free disk space for capacity decisions — the
// dispatch disk floor and the evictions it triggers.
package hostdisk

// Unknown is returned when no figure can be read (non-Linux, or no path
// that statfs answers for). Callers treat it as "no gating", the way
// hostmem's capacity guardrails fail open, so a probe failure can never
// wedge dispatch.
const Unknown = -1

// AvailableMB returns the space, in MiB, an unprivileged process can still
// write on the fullest of the filesystems holding paths, or Unknown. A path
// statfs cannot answer for (one not created yet) is skipped.
func AvailableMB(paths ...string) int { return availableMB(paths) }
