// The stop verbs — one addressed by conversation, one by blueprint run — plus
// the failure-finalization helpers a stopped or errored run uses to reach its
// final DB state + surface a toast.
//
// Stopping means one thing, and it means it for every conversation — an
// intermediate blueprint step, a final step, the only step, or a conversation
// with no blueprint at all. The agent stops, the conversation parks `open`,
// and everything outside the conversation freezes. Nothing continues until
// someone resumes it or, for task-driven work, dispositions the task.
//
// Cancellation is a real concept, but it belongs to the layers that own a
// lifecycle: the blueprint has cancel_requested / BlueprintRunStatusCancelled
// and the task has return-to-queue / mark-done. A stop that reached into the
// blueprint would make `open` mean two different things depending on a column
// the user cannot see — parked-and-resumable under a running blueprint,
// parked-and-dead under a terminal one, because the claim gate only ever
// drives steps of a running blueprint. So the stop verb leaves the blueprint
// alone and the callers that own a lifecycle one layer up spell their own
// cancellation (StopConversationAndCancelBlueprint and StopBlueprintRun, below).
//
// The park keeps the workspace, and the retention TTL is what eventually
// collects it, because the moment a user kills a wedged run is exactly the
// moment they are most likely to want the work back. The park does not WAIT
// for the workspace: the flip lands first and the snapshot follows, with a
// durable state record standing in for the blob until it exists.
//
// A frozen blueprint is the deliberate cost of that. A stopped step leaves its
// blueprint 'running' with no queued step and no live claim, holding its
// worktree, until the conversation is resumed or the task is dispositioned —
// so every surface that counts live blueprints counts it.
//
// That cost is bounded to the stopped conversation's own task, and it has to
// stay that way. Auto-delegation gates on the task, so a frozen blueprint
// holds up only its own situation; the entity's other tasks keep firing. When
// the gate keyed on the entity instead, one stopped run silently halted every
// future automated firing on that pull request — and since the firing queue
// drains off a run reaching a terminal, nothing was left to reopen it.

package delegate

import (
	"context"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/agentloop"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/toast"
)

// ErrNoActiveConversation is the answer when there is nothing to stop: the conversation
// is not visible to the caller, it already concluded, or a racing terminal
// reached the row first. It is deliberately one error for all three — telling
// an unauthorized caller which of them applies would confirm the id exists.
//
// It exists so callers can tell "nothing to stop" apart from "the stop
// failed": everything else this returns wraps an internal fault (a failed
// read, a failed park) and must not be reported as a missing run, nor echoed
// to a client.
var ErrNoActiveConversation = errors.New("no active conversation")

// The two ways StopBlueprintRun can find nothing to tear down. They are
// separate errors because they mean opposite things to a caller unwinding its
// own side effects, and that caller has to be able to say which happened:
//
//   - ErrBlueprintRunConcluded — the run reached a terminal on its own first.
//     Benign: there is no live run left to orphan, which is the outcome the
//     unwinding caller wanted anyway.
//   - ErrNoSuchBlueprintRun — no such run in this org at all. Anomalous for
//     any caller holding an id it just watched commit, and never benign.
//
// A single "nothing to stop" sentinel would force one message to cover both,
// which can only be done by overstating the first or understating the second.
var (
	ErrBlueprintRunConcluded = errors.New("blueprint run already concluded")
	ErrNoSuchBlueprintRun    = errors.New("no such blueprint run")
)

// StopCause names the lifecycle event a teardown caller is acting on — the
// thing that caller knows and the conversation itself does not. It exists so
// the note a stop leaves on the transcript can say why the work ended rather
// than only that it did: nothing resumes a conversation torn down this way,
// so that note is the entire explanation a human reading the history gets.
type StopCause string

const (
	// StopCauseTaskClosed — the system decided the task is resolved (its
	// entity merged, closed, or otherwise stopped needing attention).
	StopCauseTaskClosed StopCause = "task_closed"
	// StopCauseTaskDispositioned — a user swiped the task away.
	StopCauseTaskDispositioned StopCause = "task_dispositioned"
	// StopCauseTeamArchived — the team that owns the work was archived.
	StopCauseTeamArchived StopCause = "team_archived"
	// StopCauseFiringReverted — the firing that spawned the run was rolled
	// back, so the run should never have existed.
	StopCauseFiringReverted StopCause = "firing_reverted"
)

// note is the sentence this cause writes into the transcript. Every arm names
// the lifecycle event, because "stopped" alone is what the reader already
// knows from the row's presence.
func (c StopCause) note() string {
	switch c {
	case StopCauseTaskClosed:
		return "Run stopped: the task it was working on was closed."
	case StopCauseTaskDispositioned:
		return "Run stopped: the task it was working on was dispositioned."
	case StopCauseTeamArchived:
		return "Run stopped: the team that owns this work was archived."
	case StopCauseFiringReverted:
		return "Run stopped: the firing that started it was rolled back."
	}
	return "Run stopped: the work it belonged to was closed out."
}

// The two sentences the plain stop verb writes. A stop is not a cancellation
// and nothing concluded, so neither says so — and the user's says the one
// thing that is actually true of the conversation afterwards: it can be
// picked back up.
const (
	stopNoteByUser   = "Run stopped by the user. It may be resumed later."
	stopNoteBySystem = "Run stopped by the system."
)

// Stop ends a conversation's work at any phase — clone, fetch, worktree setup,
// or agent execution — and parks it `open`, resumable. Nothing outside the
// conversation moves: its blueprint keeps its status and its task keeps its
// disposition. The goroutine handles cleanup (worktree removal, status update).
//
// userID identifies the actor for audit. User-initiated stops
// (handler-driven) pass the requesting user's ID and the row-mark
// write routes under that user's synthetic claims. System-initiated
// stops (router cleanup, pending-firing sweeps) pass "" and the
// write routes through the admin pool. Local mode handlers pass
// runmode.LocalDefaultUserID; multi-mode handlers extract from JWT
// claims.
func (s *Spawner) Stop(orgID, conversationID, userID string) error {
	note := stopNoteBySystem
	if userID != "" {
		note = stopNoteByUser
	}
	return s.stop(orgID, conversationID, userID, false, note)
}

// StopConversationAndCancelBlueprint stops the conversation its id names and
// finalizes that conversation's blueprint_run 'cancelled' alongside it. The
// name spells both halves because only the first is addressed: the id is a
// conversation's, and the blueprint_run reached through it is a consequence.
// That second half belongs to callers that own a lifecycle one layer up and
// have already decided it is over — a task closed by the router, a task
// swiped by a user, a team archived. Nothing will resume those conversations,
// so freezing their blueprints 'running' would hold a worktree and inflate
// every live-blueprint count for work that is finished.
//
// A conversation-level stop must never route here: the terminal blueprint is
// exactly what makes a parked conversation unresumable.
//
// cause is the lifecycle event the caller is acting on. It reaches the
// transcript verbatim, so the ended conversation explains its own ending to
// whoever reads it later.
func (s *Spawner) StopConversationAndCancelBlueprint(orgID, conversationID, userID string, cause StopCause) error {
	return s.stop(orgID, conversationID, userID, true, cause.note())
}

// StopBlueprintRun tears down a whole blueprint run — every live step, the
// blueprint_run row, and the shared worktree behind them — for a system caller
// that owns a lifecycle one layer up and has decided the run should never have
// existed. It is the twin of StopConversationAndCancelBlueprint, addressed
// from the other end: that verb reaches a blueprint_run through a conversation,
// this one reaches conversations through a blueprint_run. Both ids are opaque
// strings and neither resolves against the other's table, so which verb a
// caller wants follows from which id it is holding.
//
// The cancel signal is raised before any step is enumerated: it is what stops
// the claim gate handing this blueprint's steps out, so the run cannot grow a
// step behind the teardown's back and a step minted but not yet claimed is
// covered whether or not this call sees it. A signal that fails to commit
// aborts the teardown before anything is killed — every step below it is
// pointless without it, and half of them are harmful.
//
// System-attributed throughout: the user-facing cancel of a blueprint run is
// CancelBlueprintRun, which carries the acting user's identity into its writes.
// cause names the lifecycle event and reaches every stopped step's transcript.
func (s *Spawner) StopBlueprintRun(orgID, blueprintRunID string, cause StopCause) error {
	if s.blueprints == nil {
		return fmt.Errorf("stop blueprint run: no blueprint store")
	}
	ctx := context.Background()
	br, err := s.blueprints.GetRunSystem(ctx, orgID, blueprintRunID)
	if err != nil {
		return fmt.Errorf("load blueprint run: %w", err)
	}
	if br == nil {
		return fmt.Errorf("%w %s", ErrNoSuchBlueprintRun, blueprintRunID)
	}
	if br.Status != domain.BlueprintRunStatusRunning {
		return fmt.Errorf("%w %s: status %s", ErrBlueprintRunConcluded, blueprintRunID, br.Status)
	}

	if err := s.raiseBlueprintCancel(ctx, orgID, blueprintRunID); err != nil {
		// Nothing below is worth attempting without it. Killing a step under a
		// blueprint that never learned it was cancelled doesn't stop the run —
		// the reactor reads that step's terminal, finds no cancel_requested,
		// and enqueues the next step — so a teardown that proceeded from here
		// would trade one live step for its successor and report success.
		return fmt.Errorf("raise blueprint cancel signal: %w", err)
	}

	stepIDs, err := s.blueprints.ActiveStepConversationIDsSystem(ctx, orgID, blueprintRunID)
	if err != nil {
		// The signal is committed, so the run is not forgotten: the claim gate
		// refuses a cancel-requested blueprint and the reaper finalizes it.
		// What this call can no longer do is kill the live step now, which is
		// the whole reason its caller asked.
		return fmt.Errorf("list active step conversations: %w", err)
	}
	var errs []error
	for _, id := range stepIDs {
		// ErrNoActiveConversation is a step that raced this teardown to its
		// own terminal. Not a failure: the cancel signal is already raised, so
		// the reactor reading that terminal finalizes the blueprint instead of
		// enqueuing what comes next.
		if err := s.stop(orgID, id, "", true, cause.note()); err != nil && !errors.Is(err, ErrNoActiveConversation) {
			errs = append(errs, fmt.Errorf("stop step conversation %s: %w", id, err))
		}
	}
	if len(stepIDs) == 0 {
		// No step was stopped, so nothing is going to carry this run to a
		// terminal — every path that finalizes a blueprint runs off a step's.
		// Finalize it here instead of leaving it 'running' with nothing coming,
		// holding its worktree and its task's one-active-auto-run slot.
		s.finalizeCancelledBlueprintRun(ctx, orgID, br, nil, "")
	}
	return errors.Join(errs...)
}

// stop is the shared body every stop verb routes through. cancelBlueprint gates the two places
// the blueprint layer is touched, and nothing else differs — one path, so the
// stop verb and the lifecycle teardown cannot drift apart in the parts they
// share. note is the sentence each verb writes onto the transcript.
func (s *Spawner) stop(orgID, conversationID, userID string, cancelBlueprint bool, note string) error {
	// Preflight: load the run under the caller's identity so a
	// cross-org conversationID surfaces as "not found" BEFORE we tear anything
	// down. The cancels map below is keyed only by conversationID, so without
	// this gate any caller who learns an active conversationID could fire its
	// goroutine cancel() regardless of which org owns the run — the
	// goroutine then writes the terminal row under its own captured
	// cfg.orgID and the cross-org actor is invisible to the audit
	// trail. User-initiated stops gate via the app pool under the
	// caller's claims (RLS does the visibility check); system-
	// initiated stops (router cleanup, drain sweeps) still scope
	// the read by orgID but go through the admin pool because there
	// is no user identity to project.
	var (
		conv         *domain.Conversation
		preflightErr error
	)
	if userID != "" {
		preflightErr = s.tx.SyntheticClaimsWithTx(context.Background(), orgID, userID, func(ts db.TxStores) error {
			r, e := ts.Conversations.Get(context.Background(), orgID, conversationID)
			conv = r
			return e
		})
	} else {
		conv, preflightErr = s.conversations.GetSystem(context.Background(), orgID, conversationID)
	}
	if preflightErr != nil {
		return fmt.Errorf("load conversation: %w", preflightErr)
	}
	if conv == nil {
		return fmt.Errorf("%w %s", ErrNoActiveConversation, conversationID)
	}
	// A run that already concluded has nothing to stop, and saying so here —
	// rather than letting the park write below discover it — is what keeps a
	// stale stop a pure no-op. It has to come before the blueprint signal:
	// a completed step whose blueprint is still advancing would otherwise have
	// its NEXT step cancelled by a click aimed at work that had already
	// finished.
	if domain.IsTerminalConversationStatus(conv.Status) {
		return fmt.Errorf("%w %s", ErrNoActiveConversation, conversationID)
	}

	// The stop's record is a transcript row, not a verdict on the
	// conversation. It is the same delivered stop-note the engine writes for
	// its own park decisions, and it goes in here — one site above the three
	// arms below (local kill, cross-pod signal, DB-only park), because this
	// is the only place that knows who asked.
	//
	// Before the kill, deliberately. A resumed model otherwise reads a turn
	// that stops mid-sentence followed by a new message, with nothing
	// between them saying a person intervened; writing it first means the
	// explanation exists even if this pod dies in the next line.
	//
	// The plain stop verb skips an already-parked conversation: re-stopping a
	// parked row is a gesture with nothing to stop, so there is nothing to
	// record. A lifecycle teardown is the opposite — it reaches parked
	// conversations routinely, because every caller enumerates non-terminal
	// runs and `open` is non-terminal — and for those it is the first and
	// last thing the transcript will ever say about why the work ended. Skip
	// it there and a swiped or closed task silently ends a conversation with
	// no explanation at all, which is the case this note exists for.
	if cancelBlueprint || conv.Status != domain.StatusOpen {
		s.insertStopNote(orgID, conversationID, userID, note)
	}

	if cancelBlueprint {
		// The lifecycle caller's half. Raised FIRST — before anything is
		// killed and before this call's own write. Two things follow from the
		// ordering: whichever path disposes of the run (the reactor in the
		// run's own goroutine, or the DB-only write below) sees
		// cancel_requested and finalizes the blueprint 'cancelled', and the
		// claim gate stops handing this blueprint's steps out, so nothing
		// re-claims the run in the window between the kill and the finalize.
		s.requestBlueprintCancel(context.Background(), orgID, conv.BlueprintRunID)
	}

	// Route the hard-kill through the control seam: at N=1 it resolves the
	// registered ctx cancel from s.cancels; horizontal scaling swaps it for
	// a DB-signal to the executor that owns the run. A found handle SIGKILLs
	// the live process; the goroutine then observes ctx.Err() and tears its
	// engagement down, and its reactor either freezes the blueprint (the stop
	// verb) or finalizes it off the signal raised above (the lifecycle
	// teardown).
	//
	// Hastening only. Whether the process is on this pod or another, the kill
	// is a signal and the park below is the record, so this call's answer
	// changes nothing about what happens next and is deliberately not branched
	// on. Returning here on a local handle would hand the park to the dying
	// goroutine and serialize the user-visible flip behind that goroutine's
	// workspace capture — the run reporting WORKING for as long as the capture
	// took, and a follow-up inside that window refused for a row that is not
	// parked yet.
	if !s.getController().Cancel(conversationID) {
		// No local handle. Per the reply-leg contract, the kill is fire-and-
		// forget cross-pod: the DB-only write below is already the source of
		// truth and already works cross-pod, so a best-effort signal to a live
		// remote owner only HASTENS the kill — never waited on, never affects
		// this call's outcome.
		s.signalCancelBestEffort(orgID, conversationID, conv.ExecutorID)
	}

	// Park the run directly via DB, whether or not a process was just killed.
	// ParkOpen's status-NOT-IN filter handles every non-terminal state, so this
	// is also a defensive catch for any other "row not terminal" edge case —
	// including a run already parked `open` with no subprocess to kill.
	//
	// We also have to drain the task's firing queue ourselves: a stop that
	// finds no goroutine — a run parked `open`, or one owned by another pod —
	// has no defer to piggy-back on, and an auto-fired run stopped in that
	// state would leave the queue stuck until some other run on that task
	// terminated. Draining alongside a killed goroutine's own defer is safe:
	// DrainTask serializes per task and every pop is a guarded status
	// transition, so whichever runs first fires the queued intent and the other
	// finds nothing to pop. The preflight above already loaded the run, so the
	// trigger type and the task the drain is keyed on are in hand.
	triggerType := conv.TriggerType

	// User-initiated stop: write under the stopping user's
	// synthetic claims so RLS sees a legitimate user-attributed
	// transition. System-initiated stop (router cleanup, drain
	// sweeps): admin pool, no user attribution. Detached context —
	// the request that triggered the stop can be gone but the
	// park still needs to land.
	//
	// Unfenced, deliberately: this is an outside actor ending a run, not an
	// engagement ending itself. The whole point of a stop is to override
	// whichever executor holds the run — it even signals the remote owner
	// best-effort above — so gating it on claim ownership would break the
	// feature. The claim-fenced variants exist for the executor's own
	// self-park (parkConversationOpen with a claim in scope);
	// do not route this path through it.
	//
	// No snapshot is taken here, and that is a sequencing contract rather than
	// an absence. The verb's whole job is the fast half: park the row and
	// release the claim at once, so the user gets a composer the moment they
	// ask for one. It could not do the other half on a cross-pod stop anyway —
	// the process registry consulted above is per-pod and control never holds
	// the worktree — and on a same-pod stop it must not, a capture's duration
	// being exactly what the user should never wait on.
	//
	// The other half is the killed engagement's own teardown, arriving after:
	// it records that a persist is owed and writes the snapshot (see
	// parkConversationOpen). Its status flip is then refused by the claim
	// fence — the release above has already tripped it — and says so at INFO,
	// the ordinary shape of every stop on both dialects. The two can also land
	// in the other order, when the kill outruns this verb's park: the
	// engagement's fenced park succeeds and releases the claim, and the write
	// below is a deliberate re-park of an already-parked row — a content no-op
	// the park write permits on purpose, costing one repeated `open` on the
	// wire, which consumers of that event merge idempotently by contract.
	//
	// A follow-up sent before the blob lands is accepted rather than refused as
	// expired — the pending record names the persist and its writer, which is
	// what makes parking before the blob exists safe.
	var (
		flipped bool
		err     error
	)
	bgCtx := context.Background()
	// No result summary, on either arm. The summary is the run station's
	// verdict block, and a stop concluded nothing — the note written above is
	// the record. The machine code stays: it is claim-layer vocabulary the
	// claim outcome is derived from, not text anyone reads.
	park := db.ParkStopped(domain.ParkReasonUserCancelled, "")
	if userID == "" {
		park = db.ParkStopped(domain.ParkReasonSystemCancelled, "")
	}
	if userID != "" {
		err = s.tx.SyntheticClaimsWithTx(bgCtx, orgID, userID, func(ts db.TxStores) error {
			f, mErr := ts.Conversations.ParkOpen(bgCtx, orgID, conversationID, park)
			flipped = f
			return mErr
		})
	} else {
		flipped, err = s.conversations.ParkOpenSystem(bgCtx, orgID, conversationID, park)
	}
	if err != nil {
		return fmt.Errorf("park stopped conversation: %w", err)
	}
	if !flipped {
		return fmt.Errorf("%w %s", ErrNoActiveConversation, conversationID)
	}
	s.broadcastConversationUpdate(orgID, conversationID, "open")
	if cancelBlueprint {
		// This DB-only path runs only with no live orchestrator goroutine — the
		// step had parked (open), so the orchestrator already returned and no
		// reactor will see the signal raised above. Finalize the blueprint_run
		// here instead (cancel it, clean the warm worktree) so the row isn't
		// left 'running' with nothing coming. The snapshot is deliberately NOT
		// dropped: it is the parked workspace this stop just retained.
		//
		// The plain stop verb skips this entirely — a frozen blueprint is the
		// state it wants, and finalizing here is exactly what used to make a
		// stopped conversation permanently unresumable.
		s.finalizeParkedBlueprintOnCancel(bgCtx, orgID, conv, userID)
	}
	// The task's drain is keyed off the run's trigger type so the manual
	// short-circuit holds.
	s.notifyDrainer(orgID, triggerType, conv.TaskID)
	return nil
}

// insertStopNote writes the stop onto the transcript: a delivered role=user
// row carrying the stop-note subtype, the same shape the engine's own park
// notices take. Delivered rather than pending because it states what
// happened; a resumed conversation reads it in place instead of consuming it
// as input.
//
// userID routes the write the same way every other user-attributed write in
// this file does — synthetic claims for a person, the admin pool for the
// system — and lands on the row so the transcript records who stopped it.
//
// Unfenced, for the same reason the park below it is: this is an outside
// actor ending a run, not an engagement writing about itself, and the whole
// point of a stop is to override whichever executor holds the conversation.
// It is why the fenced per-claim variant is not used here.
//
// Best effort: a stop whose note failed to land is still a stop, and
// returning an error here would leave a live agent running because its
// explanation could not be written.
func (s *Spawner) insertStopNote(orgID, conversationID, userID, content string) {
	if content == "" || s.conversations == nil {
		return
	}
	ctx := context.Background()
	// Never say the same thing twice in a row. Two teardowns can reach one
	// conversation — a task closed by the router and then swiped, an archived
	// team over a task that already closed — and each enumerates every
	// non-terminal run, so the second arrives at a row the first already
	// explained. The engine's guard is reused rather than reimplemented: it
	// walks back only to the last human input, so a stop after a resume is a
	// new event and gets its own note. A failed read falls through and
	// writes: a duplicated sentence is a far smaller loss than a conversation
	// that never says why it ended.
	if rows, err := s.conversations.ListForAssemblySystem(ctx, orgID, conversationID); err == nil {
		if agentloop.HasNoticeSince(rows, content) {
			return
		}
	} else {
		delegateLog.Warn("stop-note dedupe read failed; writing the note anyway", "conversation", conversationID, "error", err)
	}
	msg := &domain.Message{
		ConversationID: conversationID,
		UserID:         userID,
		Role:           "user",
		Subtype:        domain.MessageSubtypeStopNote,
		Content:        content,
	}
	var err error
	if userID != "" {
		err = s.tx.SyntheticClaimsWithTx(ctx, orgID, userID, func(ts db.TxStores) error {
			_, ierr := ts.Conversations.InsertMessage(ctx, orgID, msg)
			return ierr
		})
	} else {
		_, err = s.conversations.InsertMessageSystem(ctx, orgID, msg)
	}
	if err != nil {
		delegateLog.Warn("record stop note failed; the stop itself still lands", "conversation", conversationID, "error", err)
	}
}

// classifyFailureKind maps a runtime error from the agent process to
// its machine-readable failure kind, via errors.Is on the chain —
// never message text. Anything that isn't the recognized memory-limit
// kill is a generic runtime crash.
func classifyFailureKind(err error) domain.ConversationFailureKind {
	if errors.Is(err, agentproc.ErrClaimMemoryLimit) {
		return domain.ConversationFailureMemoryLimit
	}
	return domain.ConversationFailureCrash
}

// failConversation records the infra-failure terminal for a run: guarded status flip,
// a failure row on the transcript, breaker + broadcast + snapshot cleanup.
//
// claimID names the engagement doing the failing, when there is one in scope
// — every path that reached the agent has it. The terminal then goes through
// the claim fence, so an executor that was reaped mid-run cannot bury a
// successor's live conversation under its own failure. Empty claimID keeps
// the unfenced behavior for the paths that have no engagement to speak for
// (cleanup and orchestration entries that never claimed the row).
//
// Returns fenced: true when the terminal was refused because the claim is
// released. Nothing was written, and the caller must not go on to react to
// the conversation's state either — the row it would read belongs to the
// successor.
func (s *Spawner) failConversation(orgID, conversationID, taskID, claimID, triggerType, creatorUserID, errMsg string, kind domain.ConversationFailureKind) (fenced bool) {
	delegateLog.Error("conversation failed", "conversation", conversationID, "error", errMsg, "failure_kind", string(kind))

	bgCtx := context.Background()

	failMsg := &domain.Message{
		ConversationID: conversationID,
		Role:           "assistant",
		Content:        "Error: " + errMsg,
		IsError:        true,
	}
	// The failure row goes in BEFORE the status flip, because the flip
	// releases the claim: on the fenced path an insert afterwards would name
	// a claim this call had just retired and be refused as if by a zombie.
	// Ordering it first also makes the fence's answer arrive before anything
	// irreversible happens — the flip below only runs for an engagement that
	// still owns the row.
	var insertErr error
	switch {
	case claimID != "":
		_, insertErr = s.conversations.InsertMessageForClaimSystem(bgCtx, orgID, claimID, failMsg)
	case triggerType == "manual":
		insertErr = s.tx.SyntheticClaimsWithTx(bgCtx, orgID, creatorUserID, func(ts db.TxStores) error {
			_, ierr := ts.Conversations.InsertMessage(bgCtx, orgID, failMsg)
			return ierr
		})
	default:
		_, insertErr = s.conversations.InsertMessageSystem(bgCtx, orgID, failMsg)
	}
	if errors.Is(insertErr, db.ErrClaimReleased) {
		// Not this engagement's run to fail anymore. Everything below writes
		// or broadcasts about a conversation a successor is driving, so the
		// whole tail is skipped — including the breaker tick and the snapshot
		// discard, which would delete the workspace that successor resumes
		// from.
		delegateLog.Error("claim fence refused the failure terminal — a successor owns this conversation; recording nothing",
			"conversation", conversationID, "claim_id", claimID, "org_id", orgID, "error", insertErr)
		return true
	}
	if insertErr != nil {
		delegateLog.Warn("failed to record failure message", "conversation", conversationID, "error", insertErr)
	}

	// Guarded — if a terminal racing path (cancel, natural completion)
	// reached the row first, leave its status in place rather than
	// clobbering.
	var markErr error
	switch {
	case claimID != "":
		_, markErr = s.conversations.MarkFailedIfActiveForClaimSystem(bgCtx, orgID, conversationID, claimID, string(kind))
	case triggerType == "manual":
		markErr = s.tx.SyntheticClaimsWithTx(bgCtx, orgID, creatorUserID, func(ts db.TxStores) error {
			_, mErr := ts.Conversations.MarkFailedIfActive(bgCtx, orgID, conversationID, string(kind))
			return mErr
		})
	default:
		_, markErr = s.conversations.MarkFailedIfActiveSystem(bgCtx, orgID, conversationID, string(kind))
	}
	if errors.Is(markErr, db.ErrClaimReleased) {
		// The release landed between the two writes. Same answer, same tail
		// to skip; the failure row already on the transcript is the one
		// artifact of this engagement that stands, and it is attributed to
		// this claim rather than the successor's.
		delegateLog.Error("claim fence refused the failure terminal — a successor owns this conversation; recording nothing further",
			"conversation", conversationID, "claim_id", claimID, "org_id", orgID, "error", markErr)
		return true
	}
	if markErr != nil {
		delegateLog.Warn("failed to mark conversation as failed", "conversation", conversationID, "error", markErr)
	}

	s.updateBreakerCounter(taskID, triggerType, "failed")
	s.broadcastConversationFailed(orgID, conversationID, kind)

	// A failed run won't resume, so drop the workspace snapshot it may have
	// written when it parked (e.g. a turn-end park that later failed
	// mid-resume). Keyed by the run's own id: for a blueprint step (whose
	// snapshot is keyed by blueprint_run_id) this is a harmless no-op and
	// terminateBlueprint owns that blob; for a run that never snapshotted it's
	// also a no-op. The single failure chokepoint covers every failConversation caller
	// (the resume goroutine's three exits among them).
	s.discardWorkspaceSnapshot(bgCtx, orgID, conversationID)

	// Surface as a sticky error toast so the user sees the failure even when
	// they're not watching the runs page. A memory-limit kill gets copy that
	// says what happened and which knob to turn instead of echoing the raw
	// error prefix; everything else truncates the message — full stderr dumps
	// don't fit in a toast card.
	if kind == domain.ConversationFailureMemoryLimit {
		toast.Error(s.wsHub, orgID, fmt.Sprintf(
			"Run %s was stopped: it exceeded its memory limit. Raise TF_CLAIM_MEMORY_LIMIT_MB if it legitimately needs more.",
			shortConversationID(conversationID)))
	} else {
		toast.Error(s.wsHub, orgID, fmt.Sprintf("Run %s failed: %s", shortConversationID(conversationID), truncateToastMsg(errMsg, 160)))
	}
	return false
}

// truncateToastMsg caps an error message at maxLen runes with an ellipsis.
// Toasts show a short body; full errors belong in the runs log.
func truncateToastMsg(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen-1]) + "…"
}
