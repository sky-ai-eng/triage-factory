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
// A stop is a request, and the verbs here write only the request. The row's
// stop intent (conversations.stop_requested_at/by) is one writer's fact and
// its status is the holder's, so the two never race: the verb records who
// asked and hastens the kill, the engagement holding the conversation settles
// the stop through its own fenced park — status and claim release in one
// transaction — and the dispatcher settles it for a conversation nobody holds.
// The claim gate refuses a conversation with a pending stop, so work that is
// queued, or requeued after its executor died, is never started again behind
// the user's back.
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

// The two answers CheckTaskUnheld gives when an executor holds a conversation
// on the task. A caller that replaces a task's run refuses on either before
// writing anything, because only the holder can stop what it is driving and
// the replacement would be refused by the one-active-run index anyway. They
// are separate so the refusal can say whether a stop is already on its way.
var (
	ErrTaskHeld     = errors.New("an agent is still running on this task; stop it and try again once it has stopped")
	ErrTaskStopping = errors.New("the agent on this task is still stopping; try again once it has stopped")
)

// CheckTaskUnheld reports whether a new run could replace the task's current
// one in the same request: nil when no live claim holds any conversation on
// the task, else ErrTaskHeld or ErrTaskStopping. A held row is one whose
// claim is unreleased, the test the settlement uses, so a row this answers
// unheld for is one SettleTaskStops will settle.
func (s *Spawner) CheckTaskUnheld(ctx context.Context, orgID, taskID string) error {
	if s.conversations == nil {
		return nil
	}
	held, err := s.conversations.HasActiveClaimForTaskSystem(ctx, orgID, taskID)
	if err != nil || !held {
		return err
	}
	ids, err := s.conversations.ActiveIDsForTaskSystem(ctx, orgID, taskID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if conv, gerr := s.conversations.GetSystem(ctx, orgID, id); gerr == nil && conv != nil && conv.StopRequestedAt != nil {
			return ErrTaskStopping
		}
	}
	return ErrTaskHeld
}

// SettleTaskStops settles, now, the stops pending on the task's conversations
// that no live claim holds: the dispatcher's settlement, narrowed to one task
// and run by a caller that has just requested those stops and is about to
// mint the task's next run. A held conversation is left to its holder.
func (s *Spawner) SettleTaskStops(ctx context.Context, orgID, taskID string) error {
	if s.conversationQueue == nil {
		return nil
	}
	settled, err := s.conversationQueue.SettleUnclaimedStopsForTaskSystem(ctx, orgID, taskID)
	if err != nil {
		return err
	}
	s.afterSettlement(ctx, settled)
	return nil
}

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
	// StopCauseTaskRequeued — a user returned the task to the queue. Its own
	// cause rather than a disposition because the task is still open: what
	// ended is this attempt at it, not the work.
	StopCauseTaskRequeued StopCause = "task_requeued"
	// StopCauseTaskDelegated — the task was handed to a new delegation. Split
	// from a disposition for the same reason a requeue is: the task is still
	// open and the work goes on, under a conversation that is not this one.
	StopCauseTaskDelegated StopCause = "task_delegated"
	// StopCauseTaskTakenOver — a person claimed a task the agent held. Also a
	// still-open task, and the sentence a reader needs is who has it now.
	StopCauseTaskTakenOver StopCause = "task_taken_over"
	// StopCauseTeamArchived — the team that owns the work was archived.
	StopCauseTeamArchived StopCause = "team_archived"
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
	case StopCauseTaskRequeued:
		return "Run stopped: the task it was working on was returned to the queue."
	case StopCauseTaskDelegated:
		return "Run stopped: the task was handed to a new delegation."
	case StopCauseTaskTakenOver:
		return "Run stopped: a person took the task over."
	case StopCauseTeamArchived:
		return "Run stopped: the team that owns this work was archived."
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

// Stop asks for a conversation's work to end at any phase — clone, fetch,
// worktree setup, or agent execution — and be parked `open`, resumable.
// Nothing outside the conversation moves: its blueprint keeps its status and
// its task keeps its disposition. It returns once the request is recorded; the
// park lands when the holder or the dispatcher settles it.
//
// userID identifies the actor: the requesting user for a handler-driven stop,
// "" for a system stop (router cleanup, pending-firing sweeps). It becomes the
// intent's actor, and so the park_reason the settlement records. Local mode
// handlers pass runmode.LocalDefaultUserID; multi-mode handlers extract it
// from JWT claims.
func (s *Spawner) Stop(orgID, conversationID, userID string) error {
	note := stopNoteBySystem
	if userID != "" {
		note = stopNoteByUser
	}
	return s.stop(orgID, conversationID, userID, false, note)
}

// StopConversationAndCancelBlueprint stops the conversation its id names and
// has that conversation's blueprint_run finalized 'cancelled' alongside it. The
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
		// The signal is committed, so the claim gate hands out none of this
		// blueprint's queued steps. What this call cannot do without the list
		// is record the step stops that carry the run to its terminal, so the
		// caller hears that the stop did not take.
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
		// No step carries an intent, so nothing is going to carry this run to
		// a terminal — every path that finalizes a blueprint runs off a
		// step's. Finalize it here instead of leaving it 'running' with nothing
		// coming, holding its worktree and its task's one-active-run slot.
		// There is no conversation to race: this is a run-status write alone.
		s.finalizeCancelledBlueprintRun(ctx, orgID, br, nil)
	}
	return errors.Join(errs...)
}

// stop is the shared body every stop verb routes through. cancelBlueprint
// gates the one place the blueprint layer is touched, and nothing else differs
// — one path, so the stop verb and the lifecycle teardown cannot drift apart
// in the parts they share. note is the sentence each verb writes onto the
// transcript.
//
// What it writes is the request and nothing else: the stop note, the
// blueprint's cancel signal when asked, and the stop intent with its hastening
// cross-pod signal. It never writes the conversation's status, never releases
// a claim and never finalizes a blueprint. Those are settlement, and settlement
// belongs to whoever holds the conversation — the engagement through its
// fenced park, or the dispatcher for a conversation no claim holds — because
// only a holder can write the status and release the claim in one
// transaction. A request path that wrote the status too would be a second
// writer of it, racing the first.
func (s *Spawner) stop(orgID, conversationID, userID string, cancelBlueprint bool, note string) error {
	ctx := context.Background()
	// Preflight: load the conversation under the caller's identity so a
	// cross-org conversationID surfaces as "not found" BEFORE anything is
	// written or cancelled. The cancels map below is keyed only by
	// conversationID, so without this gate any caller who learns an active
	// conversationID could fire its goroutine cancel regardless of which org
	// owns it. User-initiated stops read on the app pool under the caller's
	// claims (RLS does the visibility check); system-initiated stops scope the
	// read by orgID on the admin pool, having no user identity to project.
	var (
		conv         *domain.Conversation
		preflightErr error
	)
	if userID != "" {
		preflightErr = s.tx.SyntheticClaimsWithReadTx(ctx, orgID, userID, func(ts db.TxStores) error {
			r, e := ts.Conversations.Get(ctx, orgID, conversationID)
			conv = r
			return e
		})
	} else {
		conv, preflightErr = s.conversations.GetSystem(ctx, orgID, conversationID)
	}
	if preflightErr != nil {
		return fmt.Errorf("load conversation: %w", preflightErr)
	}
	if conv == nil {
		return fmt.Errorf("%w %s", ErrNoActiveConversation, conversationID)
	}
	// A run that already concluded has nothing to stop, and saying so here —
	// rather than letting the intent write discover it — is what keeps a stale
	// stop a pure no-op. It has to come before the blueprint signal: a
	// completed step whose blueprint is still advancing would otherwise have
	// its NEXT step cancelled by a click aimed at work that had already
	// finished.
	if domain.IsTerminalConversationStatus(conv.Status) {
		return fmt.Errorf("%w %s", ErrNoActiveConversation, conversationID)
	}

	// The stop's record on the transcript is a note, not a verdict on the
	// conversation. It is the same delivered stop-note the engine writes for
	// its own park decisions, and it goes in here because this is the only
	// place that knows who asked.
	//
	// Before the intent and the kill, deliberately. A resumed model otherwise
	// reads a turn that stops mid-sentence followed by a new message, with
	// nothing between them saying a person intervened; writing it first means
	// the explanation exists even if this pod dies in the next line.
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
		// The lifecycle caller's half, raised FIRST. The blueprint's
		// cancel_requested is already an intent: whichever writer settles
		// this conversation's stop reads it and finalizes the blueprint
		// 'cancelled' — the reactor behind a live engagement, or the
		// dispatcher's settlement for a conversation nobody holds — and the
		// claim gate stops handing this blueprint's steps out in the meantime.
		s.requestBlueprintCancel(ctx, orgID, conv.BlueprintRunID)
	}

	return s.requestStop(ctx, orgID, conversationID, userID)
}

// requestStop records a stop intent and hastens it. The intent is the record;
// the local cancel and the cross-pod signal only make the holder notice sooner
// than its next lease renewal would.
//
// The signal is addressed only in multi mode, only when this pod holds no
// cancel handle for the conversation — a local handle reaches the engagement
// directly — and only to
// an owner whose heartbeat is fresh: a signal to a dead executor is never
// delivered, and that stop is settled by the dispatcher once the dead claim is
// released. The intent and the signal commit together in the store, so a
// signal never exists without the intent it hastens.
//
// ErrNoActiveConversation when the conversation went terminal between the
// caller's preflight and this write.
func (s *Spawner) requestStop(ctx context.Context, orgID, conversationID, userID string) error {
	target := ""
	if s.crossPodSignalsWired() && !s.hasLocalCancelHandle(conversationID) {
		target, _ = s.resolveLiveOwner(ctx, orgID, conversationID)
	}
	requested, err := s.conversations.RequestStopSystem(ctx, orgID, conversationID, userID, target)
	if err != nil {
		return fmt.Errorf("record stop intent: %w", err)
	}
	if !requested {
		return fmt.Errorf("%w %s", ErrNoActiveConversation, conversationID)
	}
	if target != "" {
		// The doorbell for the signal the store just inserted. It carries no
		// id: the owner's apply loop rescans its unacked signals on any "new"
		// wake, and its backstop scan finds the row if this NOTIFY is lost.
		if nerr := s.notifyCtl(ctx, "new", 0); nerr != nil {
			delegateLog.Warn("notify tf_ctl for stop signal failed; the owner's backstop scan still finds it",
				"conversation", conversationID, "error", nerr)
		}
	}
	s.getController().Cancel(conversationID)
	return nil
}

// crossPodSignalsWired reports whether the conversation_signals outbox is
// wired — multi mode only. Without it there is no one to address a signal to.
func (s *Spawner) crossPodSignalsWired() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conversationSignals != nil
}

// hasLocalCancelHandle reports whether this pod registered a cancel handle for
// the conversation — an engagement running here that a local Cancel reaches.
func (s *Spawner) hasLocalCancelHandle(conversationID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.cancels[conversationID]
	return ok
}

// insertStopNote writes the stop onto the transcript: a delivered role=user
// row carrying the stop-note subtype, the same shape the engine's own park
// notices take. Delivered rather than pending because it states what
// happened; a resumed conversation reads it in place instead of consuming it
// as input.
//
// userID routes the write — synthetic claims for a person, the admin pool for
// the system — and lands on the row so the transcript records who stopped it.
//
// Unfenced: this is a note from the actor asking for the stop, not an
// engagement writing about itself, and the asker holds no claim to name.
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
// a failure row on the transcript, the boundary stamp, breaker + broadcast.
//
// claimID names the engagement doing the failing, and is required: the
// terminal goes through the claim fence and nowhere else, so an executor that
// was reaped mid-run cannot bury a successor's live conversation under its own
// failure. The store refuses an empty claimID as a released claim, which this
// reports as fenced.
//
// Returns fenced: true when the terminal was refused because the claim is
// released. Nothing was written, and the caller must not go on to react to
// the conversation's state either — the row it would read belongs to the
// successor.
func (s *Spawner) failConversation(orgID, conversationID, taskID, claimID, triggerType, errMsg string, kind domain.ConversationFailureKind) (fenced bool) {
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
	_, insertErr := s.conversations.InsertMessageForClaimSystem(bgCtx, orgID, claimID, failMsg)
	if errors.Is(insertErr, db.ErrClaimReleased) {
		// Not this engagement's run to fail anymore. Everything below writes
		// or broadcasts about a conversation a successor is driving, so the
		// whole tail is skipped — the boundary stamp and the breaker tick
		// among them, both of which would speak for the successor's run.
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
	_, markErr := s.conversations.MarkFailedIfActiveForClaimSystem(bgCtx, orgID, conversationID, claimID, string(kind))
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

	// The boundary, stamped after the terminal it belongs to: a failure ends
	// the conversation's life as its task's live one, so nothing resumes it
	// and the memory it may owe has a row to be owed against. Reached only
	// when the fence passed — a successor's conversation is not this
	// engagement's to end. Best-effort: the terminal above is the load-bearing write, and a
	// failed stamp must not turn a recorded failure into an error.
	ended, endErr := s.conversations.EndConversationSystem(bgCtx, orgID, conversationID, domain.EndedFailed)
	if endErr != nil {
		delegateLog.Warn("stamp the failure boundary on the conversation failed", "conversation", conversationID, "error", endErr)
	} else if ended != nil {
		// The memory this failure owes: the agent may have died anywhere
		// between "before its first tool call" and "one write short of its
		// file", and only the transcript says which. Rung for the row this
		// call stamped.
		s.kickMemoryOwed(orgID, ended.ID)
	}

	s.updateBreakerCounter(taskID, triggerType, "failed")
	s.broadcastConversationFailed(orgID, conversationID, kind)

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
