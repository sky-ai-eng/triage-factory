// Package routing handles event routing: task creation, auto-delegation,
// inline close checks, and entity lifecycle transitions. It replaces the
// old auto-delegate hook in internal/delegate/auto.go.
package routing

import (
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// EntityTerminatingEvents is the set of event types that trigger an entity
// lifecycle close (active → closed). When one of these fires, the entity
// transitions to closed and all its active tasks are cascade-closed with
// close_reason="entity_closed".
//
// The cascade (enumerate tasks → cancel their runs → close entity → batch-
// close tasks) is implemented as (*Router).closeEntity — the spawner
// dependency makes a free function awkward.
var EntityTerminatingEvents = map[string]bool{
	domain.EventGitHubPRMerged:     true,
	domain.EventGitHubPRClosed:     true,
	domain.EventJiraIssueCompleted: true,
}
