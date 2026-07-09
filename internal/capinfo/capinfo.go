// Package capinfo decodes a process's own Linux capability sets into
// human-readable names, purely for boot-time legibility logging: a boot
// line reporting "CapEff=cap_sys_admin,cap_net_admin" or
// "CapEff=(empty)" turns "trust the code" into "read the logs."
//
// This package makes no security decision and enforces nothing — it only
// reads and reports. The actual capability drop happens at exec, in a
// process that never runs Go code (see docker/entrypoint.sh); by the time
// any of this package's code executes, the drop (or lack of one) has
// already happened at the kernel level.
package capinfo

import "strings"

// Describe formats a capability-name list for a single log line: "(empty)"
// when there are none (the orchestrator's expected post-drop state), else
// a comma-joined, already-sorted list.
func Describe(names []string) string {
	if len(names) == 0 {
		return "(empty)"
	}
	return strings.Join(names, ",")
}
