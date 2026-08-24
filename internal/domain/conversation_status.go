package domain

// The conversation status vocabulary and its classifiers — one Go home for
// both halves of a set of names that is otherwise spelled as bare strings in
// Go, SQL and TypeScript. ParkReason, at the bottom, is the second such set:
// not a status, but the same kind of name under the same discipline.
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
// concluded, or the infrastructure died. Stopping a conversation without concluding it
// is a park (`open`), not a third terminal — a park that is never resumed IS
// the cancellation, and cancellation itself is already spelled at the task
// layer (return-to-queue, drag-to-done) and the blueprint layer
// (`cancel_requested` / BlueprintRunStatusCancelled).
//
// Keeping the sets here means "is this conversation active?" / "is this a phase?" has
// ONE definition, not a copy per handler that can drift from the model.
//
// SQL cannot import a Go const, so both dialects keep writing and scanning
// phase names as literals. What holds them together is the dual-dialect
// conformance suite: its phase coverage is derived from AllClaimPhases(), so
// a phase added here and not taught to the stores fails on both backends.
//
// TypeScript can't import them either. The frontend mirror is hand-maintained
// in frontend/src/types.ts (CLAIM_PHASES / TERMINAL_CONVERSATION_STATUSES /
// CONVERSATION_STATUSES) — codegen buys less than it costs for eleven names that
// change about once a year — and TestFrontendMirrorsConversationStatusVocabulary in
// the root package fails the build when the two sets diverge in either
// direction. Both directions matter: the drift this replaced ran the other
// way, with the frontend branching on three statuses the backend had never
// heard of.
//
// That test pins the mirror's arrays, and the frontend's own
// conversation-status/no-ghost-conversation-status ESLint rule pins the code that branches on
// them — a status comparison or `case` arm naming something outside this file
// fails the lint. The two together are what closes the loop: the arrays are
// not what component code reads.

// The claim `phase` vocabulary: the setup/parked sub-states of a LIVE
// engagement, stored on claims.phase and coalesced over the conversation's
// stored status on display reads. Empty phase = the agent process is live.
const (
	// ClaimPhaseFetching — assembling the task context the agent starts from.
	ClaimPhaseFetching = "fetching"
	// ClaimPhaseCloning — building the conversation's worktree.
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

// AllTerminalConversationStatuses returns the terminal display statuses. One set, and
// it describes stored rows as faithfully as it describes new writes: the
// retired terminals were rewritten by migration rather than carried forward as
// names every predicate has to remember (202608010002, SQLite; Postgres had no
// rows to migrate). A stored status this doesn't list is a bug, not history.
func AllTerminalConversationStatuses() []string {
	return []string{StatusCompleted, StatusFailed}
}

// AllConversationStatuses returns every value a displayed Conversation.Status may
// carry: the derived and parked states, every claim phase, every terminal.
func AllConversationStatuses() []string {
	all := []string{StatusQueued, StatusRunning, StatusOpen}
	all = append(all, AllClaimPhases()...)
	return append(all, AllTerminalConversationStatuses()...)
}

// IsTerminalConversationStatus reports whether status is a terminal
// conversation state — one the conversation never leaves.
//
// NB failed conversations are terminal regardless of failure_kind: failure_kind is a
// *classification* of the failure (memory_limit / crash / …) and is
// legitimately empty on an unclassified or legacy failed row, so a failure
// count keys on status=="failed", never on a non-empty failure_kind.
func IsTerminalConversationStatus(status string) bool {
	switch status {
	case StatusCompleted, StatusFailed:
		return true
	}
	return false
}

// ParkReason is WHY a conversation was parked `open` — the closed vocabulary
// of conversations.park_reason.
//
// It answers one question and one only: what stopped this conversation
// without concluding it. The column it lives in used to answer two, because
// the terminal write put the MODEL's stop reason (`end_turn` / `max_tokens`)
// in the same place a park put its own, so the value meant whichever thing
// happened to write last. The model's answer is a per-turn fact and now lives
// per turn, on messages.stop_reason; this one is a per-conversation fact and
// stays here.
//
// Not a failure kind: a park is not a failure. ConversationFailureKind owns "the
// infrastructure died" and counting `user_cancelled` beside `memory_limit`
// would make every failure count wrong. Not the claim's outcome either — that
// records `parked` vs `cancelled` per engagement, and a conversation
// accumulates engagements.
//
// Empty === SQL NULL: never parked, or parked before this column existed.
type ParkReason string

const (
	// ParkReasonIdle — the turn simply ended and nothing more arrived. The
	// live driver's no-conclusion turn and its idle close.
	ParkReasonIdle ParkReason = "idle"
	// ParkReasonUserCancelled — a person stopped this conversation.
	ParkReasonUserCancelled ParkReason = "user_cancelled"
	// ParkReasonSystemCancelled — TF stopped it with no user asking: a task
	// closing its conversations, a team archive's force-stop cascade, the reaper
	// finalizing a cancel-requested step under a dead executor.
	ParkReasonSystemCancelled ParkReason = "system_cancelled"
	// ParkReasonBlueprintCancelled — the blueprint behind the step was
	// cancelled, so the step it would have run is moot.
	ParkReasonBlueprintCancelled ParkReason = "blueprint_cancelled"
	// ParkReasonBlueprintTerminal — the blueprint behind the step ended on
	// its own (concluded, failed, aborted) while this child was still
	// mid-flight. Distinct from the cancel above because nobody stopped this
	// work: it was simply overtaken, which is a different thing to read on a
	// RunStation and a different thing to go looking for.
	ParkReasonBlueprintTerminal ParkReason = "blueprint_terminal"
	// ParkReasonLaunchFailed — the runtime never started, and the claim's
	// retry budget for this loss episode is spent. The conversation parks
	// rather than fails: nothing ran, so there is nothing to have failed, and
	// a message wakes it to try again.
	ParkReasonLaunchFailed ParkReason = "launch_failed"
	// ParkReasonModelNotEnabled — the model this conversation would run on is
	// one its team may no longer pick: its default, or the model the step it
	// follows ran on, dropped out of the team's enabled set. Its own reason
	// rather than launch_failed because nothing tried to launch and retrying
	// changes nothing — the fix is a person picking a model, and a park that
	// says "the runtime could not start" sends them looking at the runtime.
	ParkReasonModelNotEnabled ParkReason = "model_not_enabled"
	// ParkReasonDrained — the executor holding this conversation is draining
	// (scale-down). A forward seam: no writer yet, and the one that lands is
	// the drain trigger internal/delegate/workspace_snapshot.go documents.
	ParkReasonDrained ParkReason = "drained"
)

// AllParkReasons returns the park_reason vocabulary. Same discipline as
// AllClaimPhases: SQL cannot import a Go const, so the dual-dialect
// conformance suite derives its coverage from this set and a reason added
// here but never taught to a store fails on both backends. The frontend
// mirror is PARK_REASON_LABELS in frontend/src/lib/conversationStatus.ts, pinned by
// TestFrontendMirrorsParkReasonVocabulary — a park reason with no gloss is
// printed to a human as a raw identifier, which is what this vocabulary
// exists to stop.
func AllParkReasons() []ParkReason {
	return []ParkReason{
		ParkReasonIdle,
		ParkReasonUserCancelled,
		ParkReasonSystemCancelled,
		ParkReasonBlueprintCancelled,
		ParkReasonBlueprintTerminal,
		ParkReasonLaunchFailed,
		ParkReasonModelNotEnabled,
		ParkReasonDrained,
	}
}

// IsParkReason reports whether reason names a park. Closed-world, like
// IsClaimPhase: the empty string and anything unrecognized are NOT park
// reasons, which is what lets the migration recognize a model stop reason
// sitting in the renamed column and clear it.
func IsParkReason(reason string) bool {
	switch ParkReason(reason) {
	case ParkReasonIdle, ParkReasonUserCancelled, ParkReasonSystemCancelled,
		ParkReasonBlueprintCancelled, ParkReasonBlueprintTerminal,
		ParkReasonLaunchFailed, ParkReasonDrained:
		return true
	}
	return false
}

// IsActiveConversationStatus reports whether a conversation is in flight — claimed and setting
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
func IsActiveConversationStatus(status string) bool {
	return status == StatusRunning || IsClaimPhase(status)
}
