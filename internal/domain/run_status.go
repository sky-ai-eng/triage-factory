package domain

// The conversation status vocabulary and its classifiers — one Go home for
// both halves of a set of names that is otherwise spelled as bare strings in
// Go, SQL and TypeScript.
//
// Two vocabularies meet in one field. `Conversation.Status` carries a
// DISPLAYED status: the value the read projections produce after coalescing
// the active claim's `phase` over the stored column. That distinction is
// load-bearing — `queued` and `running` are DERIVED and never stored (the
// stored column is `open` | a terminal | NULL, see Conversation.Status in
// agent.go) — so the classifiers below are meaningful only on a value that
// came through the display ladder, never on a raw column read.
//
//	queued | fetching | cloning | agent_starting |
//	awaiting_credentials | running | open            (non-terminal)
//	completed | failed                               (terminal)
//
// The terminal half is deliberately two names with one owner each: the agent
// concluded, or the infrastructure died. Stopping a run without concluding it
// is a park (`open`), not a third terminal — a park that is never resumed IS
// the cancellation, and cancellation itself is already spelled at the task
// layer (return-to-queue, drag-to-done) and the blueprint layer
// (`cancel_requested` / BlueprintRunStatusCancelled).
//
// Keeping the sets here means "is this run active?" / "is this a phase?" has
// ONE definition, not a copy per handler that can drift from the model.
//
// SQL cannot import a Go const, so both dialects keep writing and scanning
// phase names as literals. What holds them together is the dual-dialect
// conformance suite: its phase coverage is derived from AllClaimPhases(), so
// a phase added here and not taught to the stores fails on both backends.
//
// TypeScript can't import them either. The frontend mirror is hand-maintained
// in frontend/src/types.ts (CLAIM_PHASES / TERMINAL_RUN_STATUSES /
// RUN_STATUSES) — codegen buys less than it costs for eleven names that
// change about once a year — and TestFrontendMirrorsRunStatusVocabulary in
// the root package fails the build when the two sets diverge in either
// direction. Both directions matter: the drift this replaced ran the other
// way, with the frontend branching on three statuses the backend had never
// heard of.

// The claim `phase` vocabulary: the setup/parked sub-states of a LIVE
// engagement, stored on claims.phase and coalesced over the conversation's
// stored status on display reads. Empty phase = the agent process is live.
const (
	// ClaimPhaseFetching — assembling the task context the agent starts from.
	ClaimPhaseFetching = "fetching"
	// ClaimPhaseCloning — building the run's worktree.
	ClaimPhaseCloning = "cloning"
	// ClaimPhaseAgentStarting — spawning the agent runtime.
	ClaimPhaseAgentStarting = "agent_starting"
	// ClaimPhaseAwaitingCredentials — parked with a published sidecar key,
	// waiting on the control plane to seal this engagement's bundle.
	ClaimPhaseAwaitingCredentials = "awaiting_credentials"
)

// The displayed conversation statuses that are not claim phases: the two
// derived states, the parked state, and the terminals.
const (
	// StatusQueued — work is waiting and nobody is driving it. Derived.
	StatusQueued = "queued"
	// StatusRunning — an engagement exists and the agent process is live.
	// Derived.
	StatusRunning = "running"
	// StatusOpen — a turn ended without a conclusion: parked, not executing,
	// not concluded, resumed through its own path rather than the dispatcher.
	StatusOpen = "open"

	// StatusCompleted — the agent reached a conclusion.
	StatusCompleted = "completed"
	// StatusFailed — the infrastructure under the agent died.
	StatusFailed = "failed"
)

// AllClaimPhases returns the claim `phase` vocabulary. Adding an entry here is
// the deliberate cost of the closed-world classifier below: a new phase is a
// decision about how the fleet console and the org-ops usage subset count it,
// so it should be made in this file rather than fall out of a default arm.
func AllClaimPhases() []string {
	return []string{
		ClaimPhaseFetching,
		ClaimPhaseCloning,
		ClaimPhaseAgentStarting,
		ClaimPhaseAwaitingCredentials,
	}
}

// IsClaimPhase reports whether status names a live engagement's setup/parked
// sub-state rather than a conversation-level status.
func IsClaimPhase(status string) bool {
	switch status {
	case ClaimPhaseFetching, ClaimPhaseCloning, ClaimPhaseAgentStarting, ClaimPhaseAwaitingCredentials:
		return true
	}
	return false
}

// AllTerminalRunStatuses returns the terminal display statuses.
func AllTerminalRunStatuses() []string {
	return []string{StatusCompleted, StatusFailed}
}

// AllRunStatuses returns every value a displayed Conversation.Status may
// carry: the derived and parked states, every claim phase, every terminal.
func AllRunStatuses() []string {
	all := []string{StatusQueued, StatusRunning, StatusOpen}
	all = append(all, AllClaimPhases()...)
	return append(all, AllTerminalRunStatuses()...)
}

// IsTerminalRunStatus reports whether status is a terminal run state — one the
// run never leaves. NB failed runs are terminal regardless of failure_kind:
// failure_kind is a *classification* of the failure (memory_limit / crash /
// …) and is legitimately empty on an unclassified or legacy failed row, so a
// failure count keys on status=="failed", never on a non-empty failure_kind.
func IsTerminalRunStatus(status string) bool {
	switch status {
	case StatusCompleted, StatusFailed:
		return true
	}
	return false
}

// IsActiveRunStatus reports whether a run is in flight — claimed and setting
// up or executing, occupying an executor slot. So: `running`, or any claim
// phase. `queued` (waiting, counted as queue depth instead) and `open`
// (parked) are excluded, as is every terminal.
//
// It says what active IS rather than what it is not, which makes it
// closed-world: the empty string and any unrecognized value classify as NOT
// active. The open-world spelling this replaced ("not queued, not open, not
// terminal") auto-classified each new phase correctly, but it could not tell
// a phase apart from a typo or a raw NULL — and a miscount there silently
// inflates the fleet console's live-slot number rather than failing loudly.
func IsActiveRunStatus(status string) bool {
	return status == StatusRunning || IsClaimPhase(status)
}
