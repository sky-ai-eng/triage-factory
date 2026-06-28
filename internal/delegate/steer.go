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
// pending_approval is gone (TFAC-492): runs never park for approval anymore. A
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
	// A warm process is steered in place — getProc is the liveness gate, so the
	// server stays out of s.procs entirely by routing through here.
	if s.getProc(runID) != nil {
		return s.Steer(ctx, runID, text)
	}
	// No warm process: only a resumable run can be woken. GetSystem is scoped by
	// its orgID arg, so a run in another tenant reads as absent → not steerable.
	// A getProc-nil → GetSystem race (the run registers a process between the two
	// reads) can't slip a message past: a now-running run reads as not resumable,
	// and a concurrent resume/approval that already moved the row makes
	// ResumeOpenRun's MarkResuming compare-and-swap lose the race →
	// ErrRunNotResumable. Both map to 409, so the client just re-reads and retries.
	run, err := s.agentRuns.GetSystem(ctx, orgID, runID)
	if err != nil {
		return fmt.Errorf("load run: %w", err)
	}
	if run == nil || !resumableState(run.Status, run.Outcome) {
		return ErrRunNotSteerable
	}
	return s.ResumeOpenRun(orgID, runID, text, userID)
}
