// A user's message reaching a conversation: SendMessage is the single entry the
// message endpoint calls, and below it there is one follow-up path. Writing an
// undelivered row and making sure something will come back to read it is the
// whole operation, for every runtime and every state a conversation can be
// found in — with exactly one exception, which is what SendMessage routes on:
// an SDK conversation being driven right now holds its session inside a live
// process that drains no queue, so the only way to reach it is to tell that
// process.
//
// The server never reaches into s.procs itself; this file owns that decision so
// the horizontal-scaling swap (a run's process living on another executor)
// stays behind the same call.

package delegate

import (
	"context"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ErrRunNotSteerable is returned by SendMessage when a conversation can take no
// message: nothing is driving it, nothing will be, and it is not resting
// somewhere a wake can reach — so a queued row would sit undelivered forever.
// Callers map it to 409 Conflict so the client refreshes and re-reads the run's
// state.
var ErrRunNotSteerable = errors.New("run is not steerable")

// The SDK-only resume preconditions, as errors rather than formatted strings so
// the one ladder can hand them back by name. Each is a row that cannot be
// resumed at all — no session to re-invoke, no tree to re-invoke it in, no
// model to re-invoke it on — so they read as internal faults (500) rather than
// as a state the client should re-read.
var (
	errResumeNoSessionID    = errors.New("run has no session id; cannot resume")
	errResumeNoWorktreePath = errors.New("run has no worktree path; cannot resume")
	errResumeNoModel        = errors.New("run has no model; cannot resume")
)

// The refusal ladder's rungs, named. A rung travels two ways from the single
// walk in followUpBlock: SendMessage turns it into the error a refused send
// returns, and ResumabilityFor hands the same value to the run detail read,
// where the composer renders it as the reason it is disabled. That is the whole
// reason the ladder answers with a value instead of only an error — a client
// that has to infer "can a follow-up land?" from status alone gets it wrong for
// every row whose workspace or blueprint says otherwise.
//
// Empty means no refusal: a message lands.
const (
	// ResumeBlockedNotSteerable — the conversation is resting somewhere no wake
	// reaches and nothing is draining its queue (a failed run).
	ResumeBlockedNotSteerable = "not_steerable"
	// ResumeBlockedSessionMissing / WorktreeMissing / ModelMissing — an SDK row
	// missing one of the three fields its resume re-invokes a Claude session
	// with. Never a native conversation: its messages ARE its resume state.
	ResumeBlockedSessionMissing  = "session_missing"
	ResumeBlockedWorktreeMissing = "worktree_missing"
	ResumeBlockedModelMissing    = "model_missing"
	// ResumeBlockedWorkspaceExpired — no warm worktree and no snapshot. Gone for
	// good; no later release brings it back.
	ResumeBlockedWorkspaceExpired = "workspace_expired"
	// ResumeBlockedBlueprintConcluded — the workspace survives but nothing would
	// drive it: the blueprint was cancelled, or it finished on a later step.
	ResumeBlockedBlueprintConcluded = "blueprint_concluded"
)

// resumableState reports whether a run with no warm process can be woken by a
// follow-up message. Two stored states qualify, and between them they are every
// non-failed rung a conversation can come to rest on:
//
//   - open      — a turn ended without a conclusion.
//   - completed — the agent concluded. Whatever the outcome: an abort is work a
//     human picks back up, a finish is work a human follows up on, and both
//     have a workspace to land in because every completed terminal snapshots.
//
// Outcome no longer discriminates, so this reads as status alone. It keeps the
// two-argument shape because the MarkQueuedForResume CAS it mirrors is still
// spelled over the same pair, and a caller that has an outcome in hand should
// not have to know that this predicate has stopped caring.
//
// `failed` is the exclusion: the infrastructure under the run died, so there is
// no coherent workspace to rehydrate — failRun drops whatever blob it had.
// Runs never park for approval; a terminal run that left an unresolved artifact
// (draft PR / ready review) resumes through this same path plus the feedback
// ledger, not through a parked status.
func resumableState(status, _ string) bool {
	switch status {
	case "open", "completed":
		return true
	default:
		return false
	}
}

// injectionWillFlush reports whether an injection staged against a conversation
// with no warm process will actually be read.
//
// Deliberately narrower than resumableState, and the gap is the point.
// resumableState answers "can a person wake this?", which concluded work now
// says yes to. This answers "is something already coming back to read a row
// nobody asked for?" — and for work that finished, nothing is. A follow-up may
// never arrive; until it does the staged row is a leak, and if one eventually
// does arrive it is a double delivery against the fresh run the event's own
// deferral spawned in the meantime. Staging is an automated channel and needs
// an automated reader:
//
//   - open              — a claim picks it back up as soon as input lands.
//   - completed + abort — the agent stopped mid-work; the blueprint re-opens on
//     the resume, so the work is still in flight in every sense but the row.
//
// A conversation that finished is not in that set, whatever a message could do
// to it.
func injectionWillFlush(status, outcome string) bool {
	switch status {
	case "open":
		return true
	case "completed":
		return domain.RunOutcome(outcome) == domain.RunOutcomeAbort
	default:
		return false
	}
}

// blueprintDrivableForResume is the other half of "resumable": the run's state
// says a message COULD continue it, and this says anything would actually pick
// it up. Waking a run nothing will claim strands it mid-flight — claimed by
// nobody, counted as queue depth by every counter, forever — so this refuses
// first rather than letting the flip land.
//
// It is blueprintDrivableForClaim, exactly, and that identity is the whole
// safety property: this predicate is allowed to be stricter than the claim gate
// (a refused wake writes nothing), and never more permissive (a wake the claim
// gate then declines is the strand this whole seam exists to prevent). Sharing
// one function is the only way to keep that true through the next edit.
//
// `aborted` needs no arm of its own. A blueprint aborts on the step it was
// running, and no terminal write moves current_step_index, so the aborting
// conversation IS the final step and the shared predicate already admits it.
// An EARLIER step of that same aborted blueprint must be refused — the resume's
// blueprint re-open is conditioned on the conversation's own completed+abort
// terminal, so an earlier step (completed+continue, or parked open) would flip
// to mid-flight with the blueprint still 'aborted' and its index still pointing
// past it: claimable by nothing, forever.
//
// Admin-pool read on purpose: blueprint_runs RLS hides another user's manual
// blueprint, and a teammate resuming a run must not be refused for a row they
// merely cannot see. A run with no blueprint, or a lookup that fails, is
// treated as drivable — an inability to check must not strand a resumable run,
// and the claim gate re-checks for real.
func (s *Spawner) blueprintDrivableForResume(ctx context.Context, orgID string, run *domain.Conversation) bool {
	if s.blueprints == nil || run.BlueprintRunID == "" {
		return true
	}
	br, err := s.blueprints.GetRunSystem(ctx, orgID, run.BlueprintRunID)
	if err != nil {
		delegateLog.Warn("resume: blueprint state lookup inconclusive; treating as drivable", "run", run.ID, "blueprint_run", run.BlueprintRunID, "error", err)
		return true
	}
	return blueprintDrivableForClaim(br, run.BlueprintStepIndex)
}

// blueprintDrivableForClaim is the Go mirror of blueprintDrivableSQL: given a
// blueprint_runs row already in hand, may this conversation be driven?
//
// The claim scan asks the same question in SQL; this asks it again on the far
// side of the claim, where the window between the scan and the read is
// non-zero. The two must agree — a claim the dispatcher then refuses to drive
// is a run that ping-pongs between the queue and a park.
//
// A nil blueprint is drivable: a conversation with no blueprint parent (curator
// today, interactive tomorrow) is not this gate's business, matching the SQL's
// LEFT JOIN.
func blueprintDrivableForClaim(br *domain.BlueprintRun, stepIndex *int) bool {
	if br == nil {
		return true
	}
	// Called off: the signal a cancel raises while the sequence still runs, and
	// the terminal it settles on. Both, for the reason the SQL gives.
	if br.CancelRequested || br.Status == domain.BlueprintRunStatusCancelled {
		return false
	}
	return br.Status == domain.BlueprintRunStatusRunning || isFinalBlueprintStep(br, stepIndex)
}

// isFinalBlueprintStep reports whether stepIndex names the step the blueprint
// came to rest on — the one conversation of a finished blueprint that a
// follow-up may land on. Mirrors the equality arm of blueprintDrivableSQL, and
// exists so the claim path and the resume pre-check cannot drift apart.
//
// A nil index is not final. It is the Go spelling of the SQL's NULL comparison,
// and it is the conservative answer for the same reason: a conversation that
// never recorded its position cannot prove it is the one holding the workspace.
func isFinalBlueprintStep(br *domain.BlueprintRun, stepIndex *int) bool {
	return br != nil && stepIndex != nil && *stepIndex == br.CurrentStepIndex
}

// SendMessage delivers a free-form user message to a conversation. It owns one
// routing decision — does a live SDK process need to be told? — and hands
// everything else to queueFollowUp:
//
//   - an SDK conversation with a live process, whether that process is in this
//     pod's registry or on another executor → Steer it in place through the
//     control seam.
//   - everything else → queueFollowUp: write the undelivered row, and (if the
//     conversation is parked) wake a claim for it.
//
// userID is the actor sending the message; the follow-up's writes route under
// that user's synthetic claims regardless of the conversation's original
// trigger (a follow-up is user-initiated even when the run that earned it was
// not).
func (s *Spawner) SendMessage(ctx context.Context, orgID, runID, userID, text string) error {
	// Fast path: the run's process is in THIS pod's registry → steer in place
	// with no DB read. Self-selecting — it hits in local mode and on the pod that
	// owns the process, and is a cheap miss on a multi control pod (whose
	// steerable processes all live on executors), which the runtime + status
	// routing below then handles.
	//
	// Load-bearing precondition, and it is what makes skipping the runtime read
	// safe: the registry is written in exactly one place, the SDK live driver
	// (registerProc, called only from runLiveAndDrive in live.go). A native
	// conversation therefore CANNOT be in it, so this can never send one down a
	// Claude-session resume it never had. Anything that starts registering
	// handles from a second driver has to re-read the runtime here first.
	if s.getProc(runID) != nil {
		return s.Steer(ctx, runID, text)
	}
	// No LOCAL process. Route on the conversation's runtime and STATUS, not on
	// whether the process happens to sit in this pod's memory — using the local
	// registry as the liveness gate meant a control pod saw every running run
	// (its process on an executor) as unreachable and 409'd. GetSystem is scoped
	// by its orgID arg, so a run in another tenant reads as absent → not
	// steerable.
	run, err := s.agentRuns.GetSystem(ctx, orgID, runID)
	if err != nil {
		return fmt.Errorf("load run: %w", err)
	}
	if run == nil {
		return ErrRunNotSteerable
	}
	// The one case a queued row cannot serve, and the reason the split exists at
	// all: an SDK conversation in flight (claimed, setting up or running) holds
	// its session inside a live process, and that process drains nothing — it has
	// to be handed the text. s.Steer routes through the controller, which retries
	// THIS pod's registry (covering the getProc→GetSystem race where the process
	// just registered here) and on a miss delivers over conversation_signals to
	// the owning executor. A run mid-setup whose owner has no process registered
	// yet acks "gone" and degrades to a 409, never a lost message. In local mode
	// the controller is local-only, so an active run absent from this (sole)
	// process's registry is a genuine race → ErrNoLiveProcess.
	if run.Runtime != domain.ConversationRuntimeNative && domain.IsActiveRunStatus(run.Status) {
		return s.Steer(ctx, runID, text)
	}
	return s.queueFollowUp(ctx, orgID, *run, userID, text)
}

// queueFollowUp is the follow-up path: it records the user's message as an
// undelivered row on the conversation's own transcript and, when nothing is
// coming back on its own, wakes a claim that will read it.
//
// One operation, one refusal ladder, one write, one flip — for a parked SDK
// conversation, a parked native one, a running native one and a queued native
// one alike. What differs between those is not what happens to the message but
// what already exists to read it, which is the `wake` split below.
//
// Ordering is the invariant that makes the two halves separable: the message
// row is written BEFORE the status flip. A crash between them leaves an
// undelivered row the next claim drains (nothing is lost); the reverse order
// would leave a claimable conversation with nothing to say.
//
// The out-of-band <system-note> prepends (staged injections + the artifact
// ledger) are deliberately NOT composed here. Delivery belongs to whichever
// executor claims the row, and composing the prepend now would destructively
// flush the staged-injection queue at enqueue time, losing anything staged in
// the gap before the claim; Spawner.dispatchResumeClaim composes it immediately
// before delivery instead.
func (s *Spawner) queueFollowUp(ctx context.Context, orgID string, run domain.Conversation, userID, text string) error {
	if text == "" {
		return fmt.Errorf("send: empty message")
	}
	// Every write below routes under the sending user's synthetic claims, so the
	// identity has to be here rather than resolved later.
	if userID == "" {
		return fmt.Errorf("send: empty user id")
	}

	// The gate, refused BEFORE the row is written: a queued message nothing will
	// ever deliver is worse than a refusal. followUpBlock is the same walk the
	// run read serves `resumable` from, so a composer the server left live and a
	// send the server accepts are the same decision made once.
	if block := s.followUpBlock(ctx, orgID, &run); block != "" {
		return blockedFollowUpError(block)
	}
	// Past the gate, the only question left is which half accepted it: a
	// conversation resting somewhere a wake reaches needs the flip below, and
	// one whose driver already drains its queue needs nothing further.
	wake := resumableState(run.Status, run.Outcome)

	// The write, and it is an ordinary transcript insert whatever the runtime —
	// the queue IS the transcript's undelivered tail (role='user', blank
	// subtype, delivered=false), which is the row shape RunPendingInputStore's
	// Peek/Consume read. It appends, so several messages sent before a claim
	// arrives are all delivered: the native loop folds them in as separate
	// turns, an SDK resume takes them joined into its one prompt string.
	//
	// InsertMessage rather than a bespoke insert because the broadcast below
	// needs what only the writer can give it — the DB-assigned id and
	// created_at, both stamped onto msg here. A synthesized row would stream an
	// id of 0, and the client dedups and orders by id, so two follow-ups would
	// collapse into one and a later refetch would double them against their
	// real ids.
	msg := pendingUserInput(run.ID, userID, text)
	if err := s.tx.SyntheticClaimsWithTx(ctx, orgID, userID, func(ts db.TxStores) error {
		id, iErr := ts.Conversations.InsertMessage(ctx, orgID, msg)
		if iErr != nil {
			return iErr
		}
		msg.ID = int(id)
		return nil
	}); err != nil {
		return fmt.Errorf("queue follow-up: %w", err)
	}
	// Broadcast only for a native conversation, and the reason is upstream of
	// here: the message endpoint records and broadcasts its own optimistic user
	// row for an SDK conversation (a live SDK steer writes none, so the
	// transcript would otherwise lose the turn) and skips it for a native one,
	// whose single input door is this row. Doing it again here would put the
	// same sentence in front of the user twice.
	if run.Runtime == domain.ConversationRuntimeNative {
		s.broadcastMessage(orgID, run.ID, msg)
	}

	if !wake {
		// A driver already exists (or is about to claim) and drains before its
		// next call, so the write was the delivery.
		return nil
	}
	return s.wakeParked(ctx, orgID, run, userID)
}

// pendingUserInput builds the one row every input to a conversation takes: an
// undelivered plain user message on its own transcript. That shape is not
// cosmetic — it is the entire definition of the pending-input queue, matched by
// RunPendingInputStore's `role='user' AND subtype=” AND delivered=false`
// predicate, and no DB constraint backs it (a partial unique index on
// undelivered user rows cannot exist on the shared messages table, because
// curator conversations legitimately queue several).
//
// Both producers build the row here — a user's follow-up (queueFollowUp) and a
// delegation's opening turn (mintOpeningTurn) — so the queue cannot end up with
// two definitions of what it holds.
//
// What they do NOT share is the door they write through, and that difference is
// deliberate rather than leftover. A follow-up arrives on the control plane
// where no engagement exists yet, so it writes under the sending user's
// synthetic claims and RLS is the gate. An opening turn is written by the
// executor's own loop, a detached goroutine with no request claims at all,
// where the claim fence is the gate instead (see nativeTranscript). Forcing one
// call site would mean giving up the fence on one side or inventing claims on
// the other; sharing the row is the part that was actually duplicated.
func pendingUserInput(conversationID, userID, text string) *domain.Message {
	pending := false
	return &domain.Message{
		ConversationID: conversationID,
		UserID:         userID,
		Role:           "user",
		Content:        text,
		Delivered:      &pending,
	}
}

// drainsUndeliveredInput reports whether a conversation that is NOT parked will
// read an undelivered row without being woken for it.
//
// Runtime-conditional, and the native loop's drain is the whole of it: the loop
// folds every undelivered row into the window before each model call, so from
// the claim to the conclusion, writing the row is delivering it.
//
// The SDK has no such drain. Its undelivered rows are read by exactly one
// thing, the resume dispatch, and that resumes a Claude session by id. A queued
// SDK conversation is a blueprint step waiting for its FIRST claim: it has no
// session to resume, and staging a row against it would route that claim down
// the resume path instead of running the step's mission at all. So an unparked
// SDK conversation takes no queued message — and its live half never reaches
// this question, having already gone to Steer.
func drainsUndeliveredInput(run domain.Conversation) bool {
	if run.Runtime != domain.ConversationRuntimeNative {
		return false
	}
	return domain.IsActiveRunStatus(run.Status) || run.Status == domain.StatusQueued
}

// followUpBlock is the whole read-only gate a follow-up passes before anything
// is written: may a message land on this conversation, and if not, why not.
//
// One walk, two readers. queueFollowUp turns the answer into the error a
// refused send returns; ResumabilityFor hands it to the run detail read so the
// composer can be disabled with the same reason instead of accepting text that
// will 409 on send. Sharing the walk is the point — the client cannot see two
// of the three inputs (workspace survival, blueprint drivability), so any
// second copy of this is a copy that drifts.
//
// The split below is SendMessage's own: a conversation resting somewhere a wake
// reaches walks the refusal ladder, and one that is not resting there takes a
// message only if something is already draining its queue.
func (s *Spawner) followUpBlock(ctx context.Context, orgID string, run *domain.Conversation) string {
	if !resumableState(run.Status, run.Outcome) {
		if drainsUndeliveredInput(*run) {
			return ""
		}
		return ResumeBlockedNotSteerable
	}
	return s.unwakeableFollowUpBlock(ctx, orgID, run)
}

// blockedFollowUpError is the error a refusal reaches the HTTP layer as.
// steerErrorStatus maps these onto statuses; the reasons the server also puts
// on the run read are the same rungs by another name, so a composer disabled
// for `workspace_expired` and a send refused with 410 cannot disagree.
func blockedFollowUpError(block string) error {
	switch block {
	case "":
		return nil
	case ResumeBlockedSessionMissing:
		return errResumeNoSessionID
	case ResumeBlockedWorktreeMissing:
		return errResumeNoWorktreePath
	case ResumeBlockedModelMissing:
		return errResumeNoModel
	case ResumeBlockedWorkspaceExpired:
		return ErrWorkspaceExpired
	case ResumeBlockedBlueprintConcluded:
		return ErrConversationConcluded
	default:
		return ErrRunNotSteerable
	}
}

// ResumabilityFor answers, for one conversation, the question the composer
// needs and the client cannot compute: will a follow-up be accepted? ok=false
// carries the rung that refused it — the same value SendMessage's refusal is
// built from, read off the same walk.
//
// A read, not a promise. It is the state at read time, and two users can race
// or a retention sweep can collect the snapshot between this answer and the
// send; the 409/410 on SendMessage stays the enforcement. This just stops the
// UI offering an input that has no chance.
//
// An active conversation is steerable by a route this gate never sees — its
// live process takes the text directly — so it answers yes without walking
// anything. The detail read skips active rows anyway; this keeps the function
// total for anyone who doesn't.
func (s *Spawner) ResumabilityFor(ctx context.Context, orgID string, run *domain.Conversation) (ok bool, reason string) {
	if run == nil {
		return false, ResumeBlockedNotSteerable
	}
	if domain.IsActiveRunStatus(run.Status) {
		return true, ""
	}
	block := s.followUpBlock(ctx, orgID, run)
	return block == "", block
}

// unwakeableFollowUpBlock walks the refusals a parked conversation has to
// survive before a wake is worth writing, most-permanent-first — because all of
// them are true of rows that look identically resumable from the conversation
// alone, and only the most permanent one is worth telling a person about.
//
// One ladder for both runtimes, so a given row gets the same answer for the
// same reason whatever drives it. The SDK-only rungs at the top are the sole
// exception, and they are named rather than implied.
func (s *Spawner) unwakeableFollowUpBlock(ctx context.Context, orgID string, run *domain.Conversation) string {
	// The SDK's resume re-invokes a Claude session BY ID, in the tree it ran in,
	// on the model it started with — a config change between the original
	// invocation and this wake must not switch models underneath one logical
	// conversation — so all three have to be on the row before a wake means
	// anything. A native conversation needs none of them: its message rows ARE
	// the resume state, replayed into a fresh invocation by the loop itself.
	if run.Runtime != domain.ConversationRuntimeNative {
		if run.SessionID == "" {
			return ResumeBlockedSessionMissing
		}
		if run.WorktreePath == "" {
			return ResumeBlockedWorktreeMissing
		}
		if run.Model == "" {
			return ResumeBlockedModelMissing
		}
	}
	// A workspace reaped by the retention TTL is gone for good: no warm
	// worktree, no snapshot, nothing to rebuild from, and no future release
	// brings it back. That is 410 Gone, and refusing here means no side effect —
	// without it the flip below succeeds and the conversation dies at claim time
	// as a generic failRun, turning an expected "this expired" into a destroyed
	// run. Every run stopped by an older build lands here too: the old cancel
	// path discarded the snapshot on its way out, so a migrated row has no
	// workspace by construction and inherits exactly this answer.
	if !s.workspaceRecoverable(ctx, orgID, run) {
		return ResumeBlockedWorkspaceExpired
	}
	// The workspace survives but nothing would drive it — the blueprint was
	// cancelled, or it finished on a later step and this is not the conversation
	// it came to rest on. Asked second because it is about the sequence rather
	// than the workspace, and a person whose workspace is gone is better served
	// by hearing that.
	if !s.blueprintDrivableForResume(ctx, orgID, run) {
		return ResumeBlockedBlueprintConcluded
	}
	return ""
}

// wakeParked flips a parked conversation back to `queued` so the dispatcher
// claims it again, re-opening an aborted blueprint in the same transaction.
//
// The flip alone: the message is already queued by the caller, and it is the
// claim's own drain (native) or the resume dispatch (SDK) that delivers it.
//
// The two writes are deliberately unbound from the message write. The queue
// appends, so a wake that loses the compare-and-swap to ANOTHER wake has still
// queued its message and the winner's claim carries it — which is why the lost
// flip is resolved by lostWakeOutcome rather than reported as a conflict on
// sight.
func (s *Spawner) wakeParked(ctx context.Context, orgID string, run domain.Conversation, userID string) error {
	// A completed+abort conversation's blueprint already terminated (aborted)
	// when the step stopped. Re-open it to running in the same tx as the flip so
	// the resumed step's new conclusion re-finalizes it through the normal
	// post-resume disposition. Not for claimability — an abort leaves
	// current_step_index on the step that aborted, so the finished-blueprint arm
	// of the claim gate would take it either way — but for disposition: an abort
	// is work that paused mid-plan. A blueprint that FINISHED is the opposite
	// case and stays finished; finished machinery never restarts and the
	// follow-up rides it as-is (ReopenRunForResume's status='aborted' CAS is a
	// no-op there, and on a still-running blueprint).
	reopenAbortedBlueprint := run.Status == domain.StatusCompleted && domain.RunOutcome(run.Outcome) == domain.RunOutcomeAbort

	var flipped bool
	err := s.tx.SyntheticClaimsWithTx(ctx, orgID, userID, func(ts db.TxStores) error {
		f, fErr := ts.Conversations.MarkQueuedForResume(ctx, orgID, run.ID)
		if fErr != nil {
			return fErr
		}
		flipped = f
		if !f {
			return nil // lost the wake CAS — see lostWakeOutcome
		}
		// Atomic with the flip: a failure rolls back the flip too, so the
		// conversation and its blueprint never split across the
		// resumable/terminal boundary.
		if reopenAbortedBlueprint && run.BlueprintRunID != "" {
			if _, rErr := ts.Blueprints.ReopenRunForResume(ctx, orgID, run.BlueprintRunID); rErr != nil {
				return rErr
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("flip status: %w", err)
	}
	if !flipped {
		return s.lostWakeOutcome(ctx, orgID, run.ID)
	}
	s.broadcastRunUpdate(orgID, run.ID, domain.StatusQueued)
	s.recordResumeTaskEvent(ctx, orgID, userID, run)
	// The parked step is queued again → bounce the aggregate board column back
	// to in_progress (no-op if a sibling run is still parked).
	s.recomputeTaskBoardColumn(orgID, run.TaskID)
	// Same-process fast path (local): the wake is a ~ms in-process dispatcher
	// nudge. Cross-pod there is no wake nudge by design — the claiming
	// executor's own scan-interval backstop picks the row up.
	s.wakeDispatcher()
	return nil
}

// resumeSystemPrepends assembles the out-of-band <system-note> blocks prepended
// ahead of a resuming run's user message, in their final read order
// [staged injections][artifact ledger]:
//
//   - the durable staged-injection queue flush (TFAC-501) — every producer-
//     agnostic injection staged while the run wasn't running (e.g. the new-commits
//     freshness injection), destructively flushed (delivered exactly once); and
//   - the artifact-change ledger (TFAC-493) — every artifact this run produced
//     that a human resolved while it wasn't running, derived from the artifact
//     rows (no ledger table).
//
// The artifact ledger is composed first and the staged-injection flush second, so
// the injection block lands FIRST in the returned prefix (each step writes to the
// front). Both are out-of-band <system-note> blocks, so their order relative to
// each other is cosmetic. Returns "" (prepend nothing) when neither has anything
// pending; a read failure in either source degrades to no block for that source,
// never blocking the resume.
func (s *Spawner) resumeSystemPrepends(ctx context.Context, orgID string, run *domain.Conversation) string {
	var prefix string
	if block := s.artifactLedgerForResume(ctx, orgID, run); block != "" {
		prefix = block + "\n\n"
	}
	if block := s.stagedInjectionsForResume(ctx, orgID, run.ID); block != "" {
		prefix = block + "\n\n" + prefix
	}
	return prefix
}
