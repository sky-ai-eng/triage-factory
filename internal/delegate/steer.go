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
// follow-up message. The resumable set is every non-finish parked/terminal
// state:
//
//   - open               — a turn ended without a conclusion (works today).
//   - completed + abort  — the agent voluntarily stopped; a follow-up can pick
//     the work back up (its blueprint is re-opened on resume).
//
// pending_approval is gone: runs never park for approval anymore. A
// terminal blueprint run that left an unresolved artifact (draft PR / ready
// review) is still message-resumable through the completed+abort path + the
// feedback ledger — not through a parked status.
//
// Keyed on (status, outcome), not status alone: a finish run (completed +
// outcome='finish') is deliberately excluded — resuming finish runs is a
// gray area held out to avoid snapshotting every completed run.
func resumableState(status, outcome string) bool {
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
// it up. ClaimNextRun only drives rows whose blueprint_run is 'running' and
// not cancel-requested, so waking a run under a finished blueprint would flip
// it mid-flight and strand it there — claimed by nobody, counted as queue depth
// by every counter, forever. `aborted` passes because the resume re-opens it in
// the same transaction as the flip.
//
// This matters more now that a cancelled run parks `open` instead of writing a
// terminal: `open` under a cancelled blueprint is a real and reachable state,
// and it reads resumable from the row alone. Widening the gate to a finished
// blueprint's final conversation is the resume work this epic builds toward;
// until that lands, refusing is the honest answer.
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
	if br.CancelRequested {
		return false
	}
	return br.Status == domain.BlueprintRunStatusRunning || br.Status == domain.BlueprintRunStatusAborted
}

// SendMessage delivers a free-form user message to a run, owning the
// live-vs-resume routing:
//
//   - live (a registered warm process) → Steer it in place through the control
//     seam (no DB read — the fast path).
//   - a resumable run (no warm process; open / completed+abort — see
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
		// Parked (open / completed+abort) → wake via resume-by-enqueue. Unlike the
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
