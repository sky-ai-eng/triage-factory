package sysinfo

// DiskAvailableMB returns the space, in MiB, an unprivileged process can
// still write on the fullest of the filesystems holding paths, or Unknown. A
// path statfs cannot answer for (one not created yet) is skipped. The
// dispatch disk floor and the evictions it triggers read it.
func DiskAvailableMB(paths ...string) int { return diskAvailableMB(paths) }
