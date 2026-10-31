package procinfo

import "strings"

// DescribeCaps formats a capability-name list for a single log line:
// "(empty)" when there are none (the orchestrator's expected post-drop
// state), else a comma-joined, already-sorted list.
func DescribeCaps(names []string) string {
	if len(names) == 0 {
		return "(empty)"
	}
	return strings.Join(names, ",")
}
