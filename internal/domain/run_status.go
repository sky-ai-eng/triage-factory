package domain

// Run-status classification shared by every surface that aggregates runs by
// lifecycle (the fleet console, the org-ops usage subset). These classify
// DISPLAYED statuses — the value Conversation.Status carries after the read
// projections coalesce the active claim's phase over the stored column. That
// distinction is load-bearing: `queued` and `running` are DERIVED and never
// stored (the stored column is `open` | a terminal | NULL — see
// Conversation.Status in agent.go), so these helpers are meaningful only on a
// value that came through the display ladder, never on a raw column read.
// Alongside those two, the live claim's phase names classify here, since a
// conversation setting up is as much in flight as one calling the model:
//
//	queued | fetching | cloning | agent_starting |
//	awaiting_credentials | running | open            (non-terminal)
//	completed | failed | cancelled | task_unsolvable (terminal)
//
// IsActiveRunStatus is open-world by construction — anything that is not
// queued, open, or terminal counts as in flight — which is why each new claim
// phase has classified correctly without editing this file. The cost is that
// an unrecognized value is indistinguishable from a phase and reads as
// active. Closing that world means giving the phase vocabulary one Go home to
// test against; it is spelled as bare literals across delegate, credprovision
// and the stores today.
//
// Keeping the sets here means "is this run active?" / "is this run terminal?"
// has ONE definition, not a copy per handler that can drift from the model.

// IsTerminalRunStatus reports whether status is a terminal run state — one the
// run never leaves. NB failed runs are terminal regardless of failure_kind:
// failure_kind is a *classification* of the failure (memory_limit / crash /
// …) and is legitimately empty on an unclassified or legacy failed row, so a
// failure count keys on status=="failed", never on a non-empty failure_kind.
func IsTerminalRunStatus(status string) bool {
	switch status {
	case "completed", "failed", "cancelled", "task_unsolvable":
		return true
	}
	return false
}

// IsActiveRunStatus reports whether a run is in flight — claimed and setting up
// or executing, occupying an executor slot. It excludes:
//   - "queued": not yet claimed (work waiting, counted as queue depth instead),
//   - "open": a turn ended without a conclusion — parked, NOT executing, and
//     resumed through its own path rather than the dispatcher,
//   - every terminal state.
//
// So it is true for initializing / cloning / fetching / worktree_created /
// agent_starting / running / awaiting_credentials.
func IsActiveRunStatus(status string) bool {
	switch status {
	case "queued", "open":
		return false
	}
	return !IsTerminalRunStatus(status)
}
