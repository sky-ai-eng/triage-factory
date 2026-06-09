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
	"log"
)

// ErrRunNotSteerable is returned by SendMessage when a run can take no message
// right now: it has no live process AND is not `open` (it's terminal, or a
// transient running-with-no-registered-process race). Callers map it to 409
// Conflict so the client refreshes and re-reads the run's state.
var ErrRunNotSteerable = errors.New("run is not steerable")

// SendMessage delivers a free-form user message to a run, owning the
// live-vs-open routing:
//
//   - live (a registered warm process) → Steer it in place through the control
//     seam (no DB read — the fast path).
//   - open (no warm process, status `open`) → wake it via ResumeOpenRun, which
//     re-invokes the session with the message as the next turn's input.
//   - anything else → ErrRunNotSteerable.
//
// userID is the actor sending the message; ResumeOpenRun routes the woken run's
// writes under that user's synthetic claims (a resume is user-initiated
// regardless of the run's original trigger).
func (s *Spawner) SendMessage(ctx context.Context, orgID, runID, userID, text string) error {
	// A warm process is steered in place — getProc is the liveness gate, so the
	// server stays out of s.procs entirely by routing through here.
	if s.getProc(runID) != nil {
		log.Printf("[steer-debug] SendMessage run=%s -> live process found, STEER in place", runID)
		return s.Steer(ctx, runID, text)
	}
	// No warm process: only an `open` run can be woken. GetSystem is scoped by
	// its orgID arg, so a run in another tenant reads as absent → not steerable.
	// A getProc-nil → GetSystem race (the run registers a process between the two
	// reads) can't slip a message past: a now-running run reads status != "open"
	// → not steerable, and a concurrent resume that already flipped open →
	// running makes ResumeOpenRun's MarkResuming lose the race →
	// ErrRunNotResumable. Both map to 409, so the client just re-reads and retries.
	run, err := s.agentRuns.GetSystem(ctx, orgID, runID)
	if err != nil {
		return fmt.Errorf("load run: %w", err)
	}
	if run == nil || run.Status != "open" {
		st := "<nil run>"
		if run != nil {
			st = run.Status
		}
		log.Printf("[steer-debug] SendMessage run=%s -> NOT steerable (no live process; status=%s)", runID, st)
		return ErrRunNotSteerable
	}
	log.Printf("[steer-debug] SendMessage run=%s -> no live process, status=open, RESUME", runID)
	return s.ResumeOpenRun(orgID, runID, text, userID)
}
