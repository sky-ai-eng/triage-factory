// Generic staged-injection queue (TFAC-501). The producer-agnostic "deliver live or
// stage for next resume" seam, generalized from the artifact-change feedback
// TFAC-493 shipped:
//
//   - Live run (a warm, steerable process): StageOrDeliverInjection steers a single
//     <system-note> into it immediately, fire-and-forget (deliverInjectionLive).
//   - Terminal / paused run: the bare injection is persisted to the durable
//     staged_agent_injections queue (db.StagedInjectionStore) and flushed — bundled into
//     one <system-note> ahead of the user's message — on the next resume
//     (stagedInjectionsForResume, wired into SendMessage).
//
// Unlike the artifact-change ledger (which DERIVES its terminal injections from the
// artifact rows, so it needs no queue), a producer here has no durable row of its
// own to re-derive from — the new-commits notifier is the first such producer.
// That's why this half is a real durable queue, not a derivation.

package delegate

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// StageOrDeliverInjection routes one agent-facing injection for a run by its live state —
// the generic producer entry every staged-injection source calls:
//
//   - live (a warm process): the injection is wrapped and steered in immediately,
//     fire-and-forget (deliverInjectionLive); returns delivered=true.
//   - not live (parked / terminal-resumable / terminal): the bare body is
//     appended to the durable queue for the next resume to flush; returns
//     delivered=false.
//
// producer tags the queued row for audit (domain.StagedInjectionProducer*); body is
// the bare, already-rendered injection line (the live path wraps one, the flush
// bundles + wraps the block). A blank body is a no-op (delivered=false). Never
// flips run status or wakes a terminal run — this is an async sidecar
// (TFAC-379 #2): an injection for a terminal-but-resumable run waits in the queue until
// the user's next follow-up, and an injection for a truly terminal run that can never
// resume simply never reaches the agent (the caller gates that — see
// HandlePRNewCommits, which only stages for live-or-resumable runs).
//
// A false return is not necessarily a staged injection: the live gate may have raced a
// closing process, or (when the caller chose to stage) the append may have failed.
// Callers that need the durable guarantee check the store error; HandlePRNewCommits
// treats a drop as acceptable (the next poll re-diffs the head).
func (s *Spawner) StageOrDeliverInjection(orgID, runID, producer, body string) (delivered bool) {
	delivered, _ = s.stageOrDeliverInjection(orgID, runID, producer, body)
	return delivered
}

// StageOrDeliverInjectionResult is StageOrDeliverInjection with a
// disambiguated return, added for TFAC-594's additive-injection gate: a
// bare bool can't tell "durably staged for the next resume" (staged=true)
// apart from "dropped" (both false — no live process, AND the durable
// append itself failed: the store isn't wired, or AppendSystem errored).
// HandlePRNewCommits and any other bare-bool producer keep calling
// StageOrDeliverInjection unchanged and tolerate a drop (the next signal
// re-stages); the additive gate can't tolerate one — a dropped additive
// event has no durable row to fall back on, so it must fall through to
// pending_firings instead. Same side effects and body/producer semantics
// as StageOrDeliverInjection; the two share stageOrDeliverInjection.
func (s *Spawner) StageOrDeliverInjectionResult(orgID, runID, producer, body string) (delivered, staged bool) {
	return s.stageOrDeliverInjection(orgID, runID, producer, body)
}

func (s *Spawner) stageOrDeliverInjection(orgID, runID, producer, body string) (delivered, staged bool) {
	if body == "" {
		return false, false
	}
	if s.getProc(runID) != nil {
		s.deliverInjectionLive(orgID, runID, domain.WrapSystemNote(body))
		return true, false
	}
	if s.stagedInjections == nil {
		delegateLog.Warn("stage injection dropped: no staged-injection store wired", "run", runID, "producer", producer)
		return false, false
	}
	if err := s.stagedInjections.AppendSystem(context.Background(), orgID, &domain.StagedInjection{
		RunID:    runID,
		Producer: producer,
		Body:     body,
	}); err != nil {
		delegateLog.Warn("stage injection: append failed (the producer's next signal re-stages)", "run", runID, "producer", producer, "error", err)
		return false, false
	}
	return false, true
}

// stagedInjectionsForResume flushes a resuming run's durable staged-injection queue and
// returns the bundled <system-note> block to prepend ahead of its user message —
// every injection staged while the run was not running (TFAC-501). Returns ""
// (prepend nothing) when nothing is staged or on any read error: feedback never
// blocks a resume.
//
// The flush is destructive (DELETE … RETURNING), so an injection is delivered exactly
// once even if two resumes race. This is the durable sibling of
// artifactLedgerForResume — SendMessage prepends both, so a resume that has both
// a resolved artifact and a staged injection carries two <system-note> blocks ahead of
// the user's text.
func (s *Spawner) stagedInjectionsForResume(ctx context.Context, orgID, runID string) string {
	if s.stagedInjections == nil {
		return ""
	}
	injections, err := s.stagedInjections.FlushPendingSystem(ctx, orgID, runID)
	if err != nil {
		delegateLog.Warn("staged-injection flush failed; resuming without the block", "run", runID, "error", err)
		return ""
	}
	return domain.StagedInjectionBlock(injections)
}
