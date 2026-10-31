package delegate

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/host"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// DefaultDispatchDiskFloorMultiMB is the free space, in MiB, below which a
// multi-mode executor stops claiming conversations and evicts parked
// workspace trees ahead of their idle window. Nothing bounds the total one run
// writes to its run tree and /tmp (the jail's file-size limit bounds each
// file), so the floor is what keeps a disk that runs have nearly filled from
// taking more of them, and what leaves room for the executor's own writes:
// bare clones and snapshot staging. On a single-host install the same disk
// holds the database.
const DefaultDispatchDiskFloorMultiMB = 10240

// ParseDispatchDiskFloorMB interprets TF_DISPATCH_DISK_FLOOR_MB. Empty is the
// mode's default: DefaultDispatchDiskFloorMultiMB in multi, and 0 (disabled)
// in local, where the disk is the user's own and parked trees stay unless the
// user opts into eviction (ParseWorkspaceEvictAfter). 0 disables. A negative
// or non-numeric value returns the mode's default plus an error the caller
// logs; a bad value must not brick boot.
//
// localMode is a parameter rather than a runmode.Current() read so both
// defaults are testable without env manipulation.
func ParseDispatchDiskFloorMB(raw string, localMode bool) (int, error) {
	def := 0
	if !localMode {
		def = DefaultDispatchDiskFloorMultiMB
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return def, fmt.Errorf(
			"invalid TF_DISPATCH_DISK_FLOOR_MB %q (want an integer >= 0; 0 disables); using default %d",
			raw, def)
	}
	return n, nil
}

// SetDispatchDiskFloor sets the dispatch disk guardrail (MiB of free space
// below which claims are deferred and parked trees evicted). Call once at
// startup before RunDispatcher runs; 0 disables.
func (s *Spawner) SetDispatchDiskFloor(mb int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.diskFloorMB = mb
}

// dispatchDiskGated reports whether free disk is below the floor, logging
// only on state transitions. Fails open like dispatchMemGated: a disabled
// floor or an unreadable probe never gates.
func (s *Spawner) dispatchDiskGated() bool {
	s.mu.Lock()
	floor := s.diskFloorMB
	probe := s.diskAvailMB
	s.mu.Unlock()
	if floor <= 0 || probe == nil {
		return false
	}
	avail := probe()
	if avail == host.Unknown {
		return false
	}
	gated := avail < floor
	if s.diskGated.CompareAndSwap(!gated, gated) {
		if gated {
			dispatchLog.Warn("dispatch paused: free disk below floor; queued conversations deferred and parked workspaces evicted until it recovers",
				"available_mb", avail, "floor_mb", floor, "env", "TF_DISPATCH_DISK_FLOOR_MB")
		} else {
			dispatchLog.Info("dispatch resumed: free disk recovered above floor",
				"available_mb", avail, "floor_mb", floor)
		}
	}
	return gated
}

// executorDiskAvailableMB is the free space the disk floor reads: the least
// of the filesystem holding the run trees and their snapshot staging and the
// state root's, which holds every jail's /tmp and the bare clones. In multi
// the runs base is under the state root, so the two are normally one
// filesystem; locally the run trees are in the temp dir.
func executorDiskAvailableMB() int {
	dirs := []string{worktree.RunsBase()}
	if root, err := paths.StateRootErr(); err == nil {
		dirs = append(dirs, root)
	}
	return host.DiskAvailableMB(dirs...)
}
