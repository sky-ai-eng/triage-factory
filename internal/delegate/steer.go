// Steering a run from the API: SendMessage is the single routing entry the P3
// message endpoint calls. It decides where a user's message goes from the run's
// live state — a warm process is steered in place, an `open` run is woken via
// the durable resume, and anything else can take no message. The server never
// reaches into s.procs itself; this method owns that decision so the
// horizontal-scaling swap (a run's process living on another executor) stays
// behind the same call.

package delegate

import (
	"context"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ErrRunNotSteerable is returned by SendMessage when a run can take no message
// right now: it has no live process AND is not in a resumable state (it's
// terminal-finish, failed/cancelled, or a transient running-with-no-registered-
// process race). Callers map it to 409 Conflict so the client refreshes and
// re-reads the run's state.
var ErrRunNotSteerable = errors.New("run is not steerable")

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
// It is blueprintDrivableForClaim plus one arm, and it has to stay that:
// widening only the claim gate leaves the refusal here, and widening only this
// strands the row. The extra arm is `aborted`, which passes because the resume
// re-opens the blueprint to running in the same transaction as the flip — the
// claim gate never sees an aborted blueprint on this path.
//
// What refuses is a blueprint that was called off. It leaves parked
// conversations that read resumable from the conversation row alone and that
// nothing will ever claim, and that is the whole of ErrConversationConcluded
// now.
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
	if br == nil {
		return true
	}
	if br.Status == domain.BlueprintRunStatusAborted && !br.CancelRequested {
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

// SendMessage delivers a free-form user message to a run, owning the
// live-vs-resume routing:
//
//   - live (a registered warm process) → Steer it in place through the control
//     seam (no DB read — the fast path).
//   - a resumable run (no warm process; open / completed — see
//     resumableState) → wake it via ResumeOpenRun, which re-invokes the
//     session with the message as the next turn's input.
//   - anything else → ErrRunNotSteerable.
//
// userID is the actor sending the message; ResumeOpenRun routes the woken run's
// writes under that user's synthetic claims (a resume is user-initiated
// regardless of the run's original trigger).
func (s *Spawner) SendMessage(ctx context.Context, orgID, runID, userID, text string) error {
	// Fast path: the run's process is in THIS pod's registry → steer in place
	// with no DB read. Self-selecting — it hits in local mode and on the pod that
	// owns the process, and is a cheap miss on a multi control pod (whose
	// steerable processes all live on executors), which the status switch below
	// then routes. This is only an optimization now, NOT the live-vs-parked
	// discriminator: that is the run's status, resolved below.
	if s.getProc(runID) != nil {
		return s.Steer(ctx, runID, text)
	}
	// No LOCAL process. Route on the run's STATUS, not on whether the process
	// happens to sit in this pod's memory — the old code used the local registry
	// as the liveness gate, so on a control pod every running run (its process on
	// an executor) fell through to "not resumable" and 409'd. GetSystem is scoped
	// by its orgID arg, so a run in another tenant reads as absent → not steerable.
	run, err := s.agentRuns.GetSystem(ctx, orgID, runID)
	if err != nil {
		return fmt.Errorf("load run: %w", err)
	}
	if run == nil {
		return ErrRunNotSteerable
	}
	switch {
	case run.Runtime == domain.ConversationRuntimeNative:
		// A native conversation has one input door, and it is the messages
		// table: the loop drains every undelivered row before each call, so
		// queueing the message IS delivering it — no process to signal, no
		// resume to schedule, no routing that depends on which executor holds
		// the work or whether it is running at all.
		//
		// A running conversation picks it up on its next drain (stamped as a
		// steer, since the model is mid-work); a parked one still needs the
		// requeue below to get an executor driving it again, which
		// queueNativeMessage handles.
		return s.queueNativeMessage(ctx, orgID, *run, userID, text)

	case domain.IsActiveRunStatus(run.Status):
		// In flight on an executor (claimed, setting up or running). s.Steer routes
		// through the controller: it retries THIS pod's registry — covering the
		// getProc→GetSystem race where the process just registered here — and, on a
		// miss, delivers over conversation_signals to the owning executor. A run mid-setup
		// whose owner has no process registered yet acks "gone" and degrades to the
		// same 409 the old path returned, never a lost message. In local mode the
		// controller is local-only, so an active run absent from this (sole)
		// process's registry is a genuine race → ErrNoLiveProcess.
		return s.Steer(ctx, runID, text)
	case resumableState(run.Status, run.Outcome):
		// Parked or concluded (open / completed) → wake via resume-by-enqueue. Unlike the
		// live-steer branch, the out-of-band <system-note> prepends (staged
		// injections + artifact ledger) are NOT composed here: resume-by-enqueue
		// (TFAC-585) defers delivery to whichever executor claims the re-queued row,
		// and composing the prepend now would destructively flush the
		// staged-injection queue at enqueue time, silently losing anything staged in
		// the gap before the claim. Spawner.dispatchResumeClaim composes it
		// immediately before delivery instead.
		return s.ResumeOpenRun(ctx, orgID, runID, text, userID)
	default:
		// queued (not yet claimed — no owner to signal, not yet parked to resume)
		// or a terminal finish/failed/cancelled run: nothing live to steer and
		// nothing parked to resume.
		return ErrRunNotSteerable
	}
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
