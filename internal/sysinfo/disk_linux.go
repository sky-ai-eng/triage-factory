//go:build linux

package sysinfo

import "golang.org/x/sys/unix"

func diskAvailableMB(paths []string) int {
	least := Unknown
	for _, p := range paths {
		var st unix.Statfs_t
		if err := unix.Statfs(p, &st); err != nil {
			continue
		}
		// Bavail, not Bfree: the blocks a filesystem reserves for root are
		// out of reach of the orchestrator and the jails, which are what
		// fill it.
		mb := int(st.Bavail * uint64(st.Bsize) >> 20)
		if least == Unknown || mb < least {
			least = mb
		}
	}
	return least
}
